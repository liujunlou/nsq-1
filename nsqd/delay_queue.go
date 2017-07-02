package nsqd

import (
	"bytes"
	"encoding/binary"
	"errors"
	"github.com/absolute8511/nsq/internal/levellogger"
	"github.com/boltdb/bolt"
	"os"
	"path"
	"strconv"
	"sync/atomic"
	"time"
)

var (
	syncedOffsetKey  = []byte("synced_offset")
	bucketDelayedMsg = []byte("delayed_message")
	bucketMeta       = []byte("meta")
)

func writeDelayedMessageToBackend(buf *bytes.Buffer, msg *Message, bq *diskQueueWriter) (BackendOffset, int32, diskQueueEndInfo, error) {
	buf.Reset()
	_, err := msg.WriteDelayedTo(buf)
	if err != nil {
		return 0, 0, diskQueueEndInfo{}, err
	}
	return bq.PutV2(buf.Bytes())
}

func getDelayedMsgDBKey(dt int, ch string, ts int64, id MessageID) []byte {
	msgKey := make([]byte, len(ch)+1+2+8+8)
	binary.BigEndian.PutUint16(msgKey[:2], uint16(dt))
	pos := 2
	msgKey[pos] = '-'
	pos++
	copy(msgKey[pos:pos+len(ch)], []byte(ch))
	pos += len(ch)
	binary.BigEndian.PutUint64(msgKey[pos:pos+8], uint64(ts))
	pos += 8
	binary.BigEndian.PutUint64(msgKey[pos:pos+8], uint64(id))
	return msgKey
}

func decodeDelayedMsgDBKey(b []byte) (uint16, int64, MessageID, string, error) {
	if len(b) < 1+2+8+8 {
		return 0, 0, 0, "", errors.New("invalid buffer length")
	}
	dt := binary.BigEndian.Uint16(b[:2])
	pos := 2
	pos++
	ch := b[pos : len(b)-8-8]
	pos += len(ch)
	ts := int64(binary.BigEndian.Uint64(b[pos : pos+8]))
	pos += 8
	id := int64(binary.BigEndian.Uint64(b[pos : pos+8]))
	return dt, ts, MessageID(id), string(ch), nil
}

func getDelayedMsgDBPrefixKey(dt int, ch string) []byte {
	msgKey := make([]byte, len(ch)+1+2)
	binary.BigEndian.PutUint16(msgKey[:2], uint16(dt))
	pos := 2
	msgKey[pos] = '-'
	pos++
	copy(msgKey[pos:pos+len(ch)], []byte(ch))
	return msgKey
}

type DelayQueue struct {
	tname     string
	fullName  string
	partition int
	backend   *diskQueueWriter
	dataPath  string
	exitFlag  int32

	msgIDCursor  MsgIDGenerator
	defaultIDSeq uint64

	needFlush   int32
	putBuffer   bytes.Buffer
	kvStore     *bolt.DB
	EnableTrace int32
}

func NewDelayQueue(topicName string, part int, dataPath string, opt *Options,
	idGen MsgIDGenerator) (*DelayQueue, error) {
	dataPath = path.Join(dataPath, "delayed_queue")
	os.MkdirAll(dataPath, 0755)
	q := &DelayQueue{
		tname:       topicName,
		partition:   part,
		putBuffer:   bytes.Buffer{},
		dataPath:    dataPath,
		msgIDCursor: idGen,
	}
	q.fullName = GetTopicFullName(q.tname, q.partition)
	backendName := getDelayQueueBackendName(q.tname, q.partition)
	// max delay message size need add the delay ts and channel name
	queue, err := NewDiskQueueWriter(backendName,
		q.dataPath,
		opt.MaxBytesPerFile,
		int32(minValidMsgLength),
		int32(opt.MaxMsgSize)+minValidMsgLength+8+255, 0)

	if err != nil {
		nsqLog.LogErrorf("topic(%v) failed to init delayed disk queue: %v , %v ", q.fullName, err, backendName)
		return nil, err
	}
	q.backend = queue.(*diskQueueWriter)
	q.kvStore, err = bolt.Open(path.Join(q.dataPath, getDelayQueueDBName(q.tname, q.partition)), 0644, nil)
	if err != nil {
		nsqLog.LogErrorf("topic(%v) failed to init delayed db: %v , %v ", q.fullName, err, backendName)
		return nil, err
	}
	err = q.kvStore.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketDelayedMsg)
		if err != nil {
			return err
		}
		_, err = tx.CreateBucketIfNotExists(bucketMeta)
		return err
	})
	if err != nil {
		nsqLog.LogErrorf("topic(%v) failed to init delayed db: %v , %v ", q.fullName, err, backendName)
		return nil, err
	}

	return q, nil
}

func (q *DelayQueue) GetFullName() string {
	return q.fullName
}

func (q *DelayQueue) GetTopicName() string {
	return q.tname
}

func (q *DelayQueue) GetTopicPart() int {
	return q.partition
}

func (q *DelayQueue) SetTrace(enable bool) {
	if enable {
		atomic.StoreInt32(&q.EnableTrace, 1)
	} else {
		atomic.StoreInt32(&q.EnableTrace, 0)
	}
}

func (q *DelayQueue) nextMsgID() MessageID {
	id := uint64(0)
	if q.msgIDCursor != nil {
		id = q.msgIDCursor.NextID()
	} else {
		id = atomic.AddUint64(&q.defaultIDSeq, 1)
	}
	return MessageID(id)
}

func (q *DelayQueue) PutDelayMessage(m *Message) (MessageID, BackendOffset, int32, BackendQueueEnd, error) {
	if atomic.LoadInt32(&q.exitFlag) == 1 {
		return 0, 0, 0, nil, errors.New("exiting")
	}
	if m.ID > 0 {
		nsqLog.Logf("should not pass id in message ")
		return 0, 0, 0, nil, ErrInvalidMessageID
	}
	if !IsValidDelayedMessage(m) {
		return 0, 0, 0, nil, errors.New("invalid delayed message")
	}

	id, offset, writeBytes, dend, err := q.put(m, true)
	return id, offset, writeBytes, &dend, err
}

func (q *DelayQueue) PutDelayMessageOnReplica(m *Message, offset BackendOffset) (BackendQueueEnd, error) {
	if atomic.LoadInt32(&q.exitFlag) == 1 {
		return nil, ErrExiting
	}
	wend := q.backend.GetQueueWriteEnd()
	if wend.Offset() != offset {
		nsqLog.LogErrorf("topic %v: write offset mismatch: %v, %v", q.GetFullName(), offset, wend)
		return nil, ErrWriteOffsetMismatch
	}
	if !IsValidDelayedMessage(m) {
		return nil, errors.New("invalid delayed message")
	}
	_, _, _, dend, err := q.put(m, false)
	if err != nil {
		return nil, err
	}
	return &dend, nil
}

func (q *DelayQueue) put(m *Message, trace bool) (MessageID, BackendOffset, int32, diskQueueEndInfo, error) {
	if m.ID <= 0 {
		m.ID = q.nextMsgID()
	}

	offset, writeBytes, dend, err := writeDelayedMessageToBackend(&q.putBuffer, m, q.backend)
	atomic.StoreInt32(&q.needFlush, 1)
	if err != nil {
		nsqLog.LogErrorf(
			"TOPIC(%s) : failed to write delayed message to backend - %s",
			q.GetFullName(), err)
		return m.ID, offset, writeBytes, dend, err
	}
	msgKey := getDelayedMsgDBKey(int(m.DelayedType), m.DelayedChannel, m.DelayedTs, m.ID)

	err = q.kvStore.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketDelayedMsg)
		exists := b.Get(msgKey) != nil
		err := b.Put(msgKey, q.putBuffer.Bytes())
		if err != nil {
			return err
		}
		b = tx.Bucket(bucketMeta)
		if !exists {
			cntKey := []byte("counter_" + strconv.Itoa(int(m.DelayedType)) + "_" + m.DelayedChannel)
			cnt := uint64(0)
			cntBytes := b.Get(cntKey)
			if cntBytes != nil && len(cntBytes) == 8 {
				cnt = binary.BigEndian.Uint64(cntBytes)
			}
			cnt++
			cntBytes = make([]byte, 8)

			binary.BigEndian.PutUint64(cntBytes[:8], cnt)
			err = b.Put(cntKey, cntBytes)
			if err != nil {
				return err
			}
		}
		return b.Put(syncedOffsetKey, []byte(strconv.Itoa(int(dend.Offset()))))
	})
	if err != nil {
		nsqLog.LogErrorf(
			"TOPIC(%s) : failed to write delayed message %v to kv store- %s",
			q.GetFullName(), m, err)
		return m.ID, offset, writeBytes, dend, err
	}
	if trace {
		if m.TraceID != 0 || atomic.LoadInt32(&q.EnableTrace) == 1 || nsqLog.Level() >= levellogger.LOG_DETAIL {
			nsqMsgTracer.TracePub(q.GetTopicName(), q.GetTopicPart(), "DELAY_QUEUE_PUB", m.TraceID, m, offset, dend.TotalMsgCnt())
		}
	}

	return m.ID, offset, writeBytes, dend, nil
}

func (q *DelayQueue) Delete() error {
	return q.exit(true)
}

func (q *DelayQueue) Close() error {
	return q.exit(false)
}

func (q *DelayQueue) exit(deleted bool) error {
	if !atomic.CompareAndSwapInt32(&q.exitFlag, 0, 1) {
		return errors.New("exiting")
	}

	q.kvStore.Close()
	if deleted {
		os.RemoveAll(path.Join(q.dataPath, getDelayQueueDBName(q.tname, q.partition)))
		return q.backend.Delete()
	}

	// write anything leftover to disk
	q.flush()
	return q.backend.Close()
}

func (q *DelayQueue) ForceFlush() {
	s := time.Now()
	q.flush()
	cost := time.Now().Sub(s)
	if cost > time.Second {
		nsqLog.Logf("topic(%s): flush cost: %v", q.GetFullName(), cost)
	}
}

func (q *DelayQueue) flush() error {
	ok := atomic.CompareAndSwapInt32(&q.needFlush, 1, 0)
	if !ok {
		return nil
	}
	err := q.backend.Flush()
	if err != nil {
		nsqLog.LogErrorf("failed flush: %v", err)
		return err
	}
	q.kvStore.Sync()
	return err
}

func (q *DelayQueue) EmptyDelayedUntil(dt int, peekTs int64, ch string) {
	db := q.kvStore
	var prefix []byte
	prefix = getDelayedMsgDBPrefixKey(dt, ch)
	db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketDelayedMsg)
		c := b.Cursor()
		for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
			_, delayedTs, _, _, err := decodeDelayedMsgDBKey(k)
			if err != nil {
				continue
			}
			if delayedTs > peekTs {
				break
			}

			b.Delete(k)
		}
		return nil
	})
}

func (q *DelayQueue) EmptyDelayedChannel(ch string) {
	db := q.kvStore
	var prefix []byte
	prefix = getDelayedMsgDBPrefixKey(1, ch)
	db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketDelayedMsg)
		c := b.Cursor()
		for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
			b.Delete(k)
		}
		return nil
	})
}

func (q *DelayQueue) PeekRecentTimeoutWithFilter(results []Message, peekTs int64, filterType int,
	filterChannel string) (int, error) {
	db := q.kvStore
	idx := 0
	var prefix []byte
	if filterType > 0 {
		prefix = getDelayedMsgDBPrefixKey(filterType, filterChannel)
	}
	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketDelayedMsg)
		c := b.Cursor()
		for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			_, delayedTs, _, _, err := decodeDelayedMsgDBKey(k)
			if err != nil {
				continue
			}
			if delayedTs > peekTs || idx >= len(results) {
				break
			}

			m, err := DecodeDelayedMessage(v)
			if err != nil {
				nsqLog.LogErrorf("failed to decode delayed message: %v, %v", m, err)
				continue
			}
			if nsqLog.Level() >= levellogger.LOG_DETAIL {
				nsqLog.LogDebugf("peek delayed message: %v, %v", delayedTs, m)
			}

			if filterType >= 0 && filterType != int(m.DelayedType) {
				continue
			}
			if len(filterChannel) > 0 && filterChannel != m.DelayedChannel {
				continue
			}
			results[idx] = *m
			idx++
		}
		return nil
	})
	return idx, err
}

func (q *DelayQueue) PeekRecentChannelTimeout(results []Message, ch string) (int, error) {
	return q.PeekRecentTimeoutWithFilter(results, time.Now().UnixNano(), 1, ch)
}

func (q *DelayQueue) PeekRecentDelayedPub(results []Message) (int, error) {
	return q.PeekRecentTimeoutWithFilter(results, time.Now().UnixNano(), 2, "")
}

func (q *DelayQueue) PeekAll(results []Message) (int, error) {
	return q.PeekRecentTimeoutWithFilter(results, time.Now().Add(time.Hour*24*365).UnixNano(), -1, "")
}

func (q *DelayQueue) GetSyncedOffset() (BackendOffset, error) {
	var synced BackendOffset
	err := q.kvStore.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		v := b.Get(syncedOffsetKey)
		offset, err := strconv.Atoi(string(v))
		if err != nil {
			return err
		}
		synced = BackendOffset(offset)
		return nil
	})
	if err != nil {
		nsqLog.LogErrorf("failed to get synced offset: %v", err)
	}
	return synced, err
}

func (q *DelayQueue) GetCurrentDelayedCnt(channel string) uint64 {
	cnt := uint64(0)
	q.kvStore.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		cntKey := []byte("counter_" + strconv.Itoa(1) + "_" + channel)
		cntBytes := b.Get(cntKey)
		if cntBytes != nil {
			cnt = binary.BigEndian.Uint64(cntBytes)
		}
		return nil
	})

	return cnt
}

func (q *DelayQueue) ConfirmedMessage(msg *Message) error {
	// confirmed message is finished by channel, this message has swap the
	// delayed id and original id to make sure the map key of inflight is original id
	msgKey := getDelayedMsgDBKey(int(msg.DelayedType), msg.DelayedChannel,
		msg.DelayedTs, msg.DelayedOrigID)

	err := q.kvStore.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketDelayedMsg)
		oldV := b.Get(msgKey)
		err := b.Delete(msgKey)
		if err != nil {
			return err
		}
		if oldV != nil {
			m, err := DecodeDelayedMessage(oldV)
			if err != nil {
				nsqLog.Infof("failed to decode delayed message: %v, %v", oldV, err)
				return err
			}
			b = tx.Bucket(bucketMeta)
			cntKey := []byte("counter_" + strconv.Itoa(int(m.DelayedType)) + "_" + m.DelayedChannel)
			cnt := uint64(0)
			cntBytes := b.Get(cntKey)
			if cntBytes != nil && len(cntBytes) == 8 {
				cnt = binary.BigEndian.Uint64(cntBytes)
			}
			if cnt > 0 {
				cnt--
				cntBytes = make([]byte, 8)
				binary.BigEndian.PutUint64(cntBytes[:8], cnt)
				err = b.Put(cntKey, cntBytes)
				if err != nil {
					return err
				}
			}
		} else {
			nsqLog.Infof("failed to get the deleting delayed message: %v, %v", msgKey, msg)
		}
		return nil
	})
	if err != nil {
		nsqLog.LogErrorf(
			"TOPIC(%s) : failed to delete delayed message %v-%v, %v",
			q.GetFullName(), msg.ID, msg.DelayedTs, err)
	}
	return err
}
