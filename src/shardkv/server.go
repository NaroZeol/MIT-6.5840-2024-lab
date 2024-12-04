package shardkv

import (
	"bytes"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"6.5840/labgob"
	"6.5840/labrpc"
	"6.5840/raft"
	"6.5840/shardctrler"
)

const Debug = false

func DPrintf(format string, a ...interface{}) (n int, err error) {
	if Debug {
		log.Printf(format, a...)
	}
	return
}

type Session struct {
	LastOp      Op
	LastOpVaild bool
	LastOpIndex int
}

type Op struct {
	Type   string
	Number int64
	ReqNum int64
	CkId   int64

	Args interface{}
}

type ShardKV struct {
	mu       sync.Mutex
	ckMu     sync.Mutex // client request mutex to block all client request when configuration changing
	me       int
	rf       *raft.Raft
	applyCh  chan raft.ApplyMsg
	make_end func(string) *labrpc.ClientEnd
	gid      int
	ctrlers  []*labrpc.ClientEnd
	mck      *shardctrler.Clerk
	config   shardctrler.Config

	dead int32

	maxraftstate int // snapshot if log grows this big
	persister    *raft.Persister

	mp          map[string]string
	ckSessions  map[int64]Session
	logRecord   map[int]Op
	confirmMap  map[int]bool
	lastApplied int

	localReqNum int64

	snapShotIndex int
}

type Snapshot struct {
	Mp         map[string]string
	CkSessions map[int64]Session
	Maker      int // for debug
}

func (kv *ShardKV) Get(args *GetArgs, reply *GetReply) {
	kv.ckMu.Lock()
	defer kv.ckMu.Unlock()

	kv.handleNormalRPC(args, reply, OT_GET)
}

func (kv *ShardKV) Put(args *PutAppendArgs, reply *PutAppendReply) {
	kv.ckMu.Lock()
	defer kv.ckMu.Unlock()

	kv.handleNormalRPC(args, reply, OT_PUT)
}

func (kv *ShardKV) Append(args *PutAppendArgs, reply *PutAppendReply) {
	kv.ckMu.Lock()
	defer kv.ckMu.Unlock()

	kv.handleNormalRPC(args, reply, OT_APPEND)
}

func (kv *ShardKV) ChangeConfig(args *ChangeConfigArgs, reply *ChangeConfigReply) {
	// should **NOT** hold kv.mu when this function is called by local machine
	kv.ckMu.Lock()
	defer kv.ckMu.Unlock()

	DPrintf("[SKV-S][%v][%v] receive ChangeConfig RPC from [%v][%v], start MoveShards", kv.gid, kv.me, args.Gid, args.Me)
	kv.MoveShards(kv.config, args.Config)
	kv.config = args.Config

	reply.Err = OK
	reply.Num = kv.config.Num
}

func (kv *ShardKV) RequestMap(args *RequestMapArgs, reply *RequestMapReply) {
	mpdup := make(map[string]string)

	if _, isLeader := kv.rf.GetState(); !isLeader {
		reply.Err = ERR_WrongLeader
		return
	}

	kv.mu.Lock()
	if kv.config.Num < args.Config.Num {
		// TODO
	} else {
		kv.mu.Unlock()
	}

	kv.mu.Lock()
	for key, value := range kv.mp {
		mpdup[key] = value
	}
	kv.mu.Unlock()

	reply.Err = OK
	reply.Mp = mpdup
}

func (kv *ShardKV) handleNormalRPC(args GenericArgs, reply GenericReply, opType string) {
	DPrintf("[SKV-S][%v][%v] receive RPC %v: %+v", kv.gid, kv.me, opType, args)

	kv.mu.Lock()
	if args.getId() != Local_ID && kv.config.Shards[key2shard(args.getKey())] != kv.gid {
		reply.setErr(ERR_WrongGroup)
		DPrintf("[SKV-S][%v][%v] Group [%v] reply with error: %v", kv.gid, kv.me, kv.gid, ERR_WrongGroup)
		kv.mu.Unlock()
		return
	}

	if session := kv.ckSessions[args.getId()]; session.LastOpVaild && session.LastOp.ReqNum == args.getReqNum() {
		if op, ok := kv.logRecord[session.LastOpIndex]; ok && op.Number == session.LastOp.Number {
			kv.successCommit(args, reply, opType)
			kv.mu.Unlock()
			return
		} // else start a new operation
	}

	op := Op{
		Type:   opType,
		Number: nrand(),
		ReqNum: args.getReqNum(),
		CkId:   args.getId(),
	}

	switch opType {
	case OT_GET:
		op.Args = *args.(*GetArgs)
	case OT_PUT:
		op.Args = *args.(*PutAppendArgs)
	case OT_APPEND:
		op.Args = *args.(*PutAppendArgs)
	}

	index, _, isLeader := kv.rf.Start(op)
	if !isLeader {
		reply.setErr(ERR_WrongLeader)
		DPrintf("[SKV-S][%v][%v] failed to Start(), not a leader", kv.gid, kv.me)
		kv.mu.Unlock()
		return
	} else {
		DPrintf("[SKV-S][%v][%v] Start #%v", kv.gid, kv.me, index)
	}
	kv.mu.Unlock()

	kv.waittingForCommit(op, index, args, reply, opType)
}

func (kv *ShardKV) waittingForCommit(op Op, index int, args GenericArgs, reply GenericReply, opType string) {
	startTime := time.Now()
	for !kv.killed() {
		kv.mu.Lock()
		if index <= kv.lastApplied {
			finishedOp, ok := kv.logRecord[index]
			if ok && finishedOp.Number == op.Number {
				kv.successCommit(args, reply, opType)
				kv.mu.Unlock()
				return
			} else if ok && finishedOp.Number != op.Number {
				DPrintf("[SKV-S][%v][%v] Failed to commit op #%v, wrong Op.number", kv.gid, kv.me, index)
				reply.setErr(ERR_FailedToCommit)
				kv.mu.Unlock()
				return
			}
		}
		kv.mu.Unlock()

		if time.Since(startTime) > 30*time.Millisecond {
			DPrintf("[SKV-S][%v][%v] Failed to commit op #%v, timeout", kv.gid, kv.me, index)
			reply.setErr(ERR_CommitTimeout)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (kv *ShardKV) successCommit(args GenericArgs, reply GenericReply, opType string) {
	// should hold kv.mu

	switch opType {
	case OT_GET:
		getArgs := args.(*GetArgs)
		getReply := reply.(*GetReply)

		getReply.Err = OK
		getReply.Value = kv.mp[getArgs.Key]
	case OT_PUT:
		fallthrough
	case OT_APPEND:
		putAppendReply := reply.(*PutAppendReply)
		putAppendReply.Err = OK
	default:
		log.Fatal("Wrong switch in successCommit()")
	}
}

func (kv *ShardKV) applyOp(op Op) bool {
	// applyMsg() -> applyOp()
	// should hold kv.mu

	switch op.Type {
	case OT_GET:
		getArgs := op.Args.(GetArgs)
		if kv.config.Shards[key2shard(getArgs.Key)] == kv.gid {
			DPrintf("[SKV-S][%v][%v] Apply Op: Get(%v)", kv.gid, kv.me, getArgs.Key)
			return true
		} else {
			DPrintf("[SKV-S][%v][%v] Failed to apply Op: Get(%v)", kv.gid, kv.me, getArgs.Key)
			return false
		}
	case OT_PUT:
		putArgs := op.Args.(PutAppendArgs)
		if kv.config.Shards[key2shard(putArgs.Key)] == kv.gid {
			kv.mp[putArgs.Key] = putArgs.Value
			DPrintf("[SKV-S][%v][%v] Apply Op: Put(%v, %v)", kv.gid, kv.me, putArgs.Key, putArgs.Value)
			return true
		} else {
			DPrintf("[SKV-S][%v][%v] Failed to apply Op: Put(%v, %v)", kv.gid, kv.me, putArgs.Key, putArgs.Value)
			return false
		}
	case OT_APPEND:
		appendArgs := op.Args.(PutAppendArgs)
		if kv.config.Shards[key2shard(appendArgs.Key)] == kv.gid {
			kv.mp[appendArgs.Key] = kv.mp[appendArgs.Key] + appendArgs.Value
			DPrintf("[SKV-S][%v][%v] Apply Op: Append(%v, %v)", kv.gid, kv.me, appendArgs.Key, appendArgs.Value)
			return true
		} else {
			DPrintf("[SKV-S][%v][%v] Failed to apply Op: Append(%v, %v)", kv.gid, kv.me, appendArgs.Key, appendArgs.Value)
			return false
		}

	default:
		log.Fatal("wrong switch in applyOp")
	}

	// unreachable
	return false
}

func (kv *ShardKV) MoveShards(oldConfig shardctrler.Config, newConfig shardctrler.Config) {
	// should hold kv.mu
	// TODO: fix dead lock

	// do nothing if it's this first config
	if oldConfig.Num == 0 {
		return
	}

	type pair struct { // I miss C++'s Pair
		gid   int
		shard int
	}

	receiveFrom := make([]pair, 0) // which shard should receive from. (gid -> shard)

	for i := 0; i < shardctrler.NShards; i++ {
		if oldConfig.Shards[i] != kv.gid && newConfig.Shards[i] == kv.gid {
			receiveFrom = append(receiveFrom, pair{
				gid:   oldConfig.Shards[i],
				shard: i,
			})
		}
	}
	DPrintf("[SKV-S][%v][%v] oldConfig: %+v", kv.gid, kv.me, oldConfig)
	DPrintf("[SKV-S][%v][%v] newConfig: %+v", kv.gid, kv.me, newConfig)
	DPrintf("[SKV-S][%v][%v] receiveFrom: %+v", kv.gid, kv.me, receiveFrom)

	// receive From
	// terrible code style
	// TODO: rebuild
	wg := sync.WaitGroup{}
	for _, pa := range receiveFrom {
		gid := pa.gid // I miss tuple...
		shard := pa.shard

		wg.Add(1)
		go func(gid int, shard int) {
			defer wg.Done()

			for {
				if servers, ok := oldConfig.Groups[gid]; ok {
					for si := 0; si < len(servers); si++ {
						srv := kv.make_end(servers[si])
						var args RequestMapArgs
						var reply RequestMapReply
						ok := srv.Call("ShardKV.RequestMap", &args, &reply)

						if ok && reply.Err == OK {
							kv.mu.Lock()
							for key, value := range reply.Mp {
								if key2shard(key) == shard {
									kv.mp[key] = value
									DPrintf("[SKV-S][%v][%v] Set key: %v, value: %v", kv.gid, kv.me, key, value)
								}
							}
							kv.mu.Unlock()
							DPrintf("[SKV-S][%v][%v] RequestMap from Server[%v][%v] sucessfully", kv.gid, kv.me, gid, si)
							return
						}
						// ... not ok, or ErrWrongLeader
						if ok && (reply.Err != OK) {
							DPrintf("[SKV-S][%v][%v] Server [%v][%v] reply with error: %v", kv.gid, kv.me, gid, si, reply.Err)
							continue
						}
					}
				}
				time.Sleep(100 * time.Millisecond)
			}
		}(gid, shard)
	}
	kv.mu.Unlock()
	wg.Wait()

	kv.mu.Lock()
}

func (kv *ShardKV) handleApplyMsg() {
	for applyMsg := range kv.applyCh {
		if applyMsg.CommandValid {
			kv.mu.Lock()
			op := applyMsg.Command.(Op)
			if kv.lastApplied+1 != applyMsg.CommandIndex {
				DPrintf("[SKV-S][%v][%v] apply out of order", kv.gid, kv.me)
			}

			kv.logRecord[applyMsg.CommandIndex] = op
			kv.lastApplied = applyMsg.CommandIndex

			// stable operation, don't change state machine
			if session := kv.ckSessions[op.CkId]; session.LastOpVaild && session.LastOpIndex < applyMsg.CommandIndex &&
				op.ReqNum <= session.LastOp.ReqNum {
				DPrintf("[SKV-S][%v][%v] stable operation #%v for [%v] ($%v <= $%v), do not change state machine", kv.gid, kv.me, applyMsg.CommandIndex, op.CkId, op.ReqNum, session.LastOp.ReqNum)

				session.LastOp = op
				session.LastOpIndex = applyMsg.CommandIndex
				session.LastOpVaild = true
				kv.ckSessions[op.CkId] = session
				kv.mu.Unlock()
				continue
			}

			if !kv.applyOp(op) { // failed to apply due to shards move
				// let waittingForCommit() failed.
				// emm...is this OK?
				op.Number = -1
			}

			s := kv.ckSessions[op.CkId]
			s.LastOp = op
			s.LastOpIndex = applyMsg.CommandIndex
			s.LastOpVaild = true
			kv.ckSessions[op.CkId] = s

			if kv.maxraftstate != -1 && kv.persister.RaftStateSize() >= kv.maxraftstate {
				DPrintf("[SKV-S][%v][%v] %v >= %v try to create snapshot up to #%v", kv.gid, kv.me, kv.persister.RaftStateSize(), kv.maxraftstate, applyMsg.CommandIndex)
				for key := range kv.logRecord {
					if kv.confirmMap[key] {
						delete(kv.logRecord, key)
						delete(kv.confirmMap, key)
					}
				}

				newSnapshot := Snapshot{
					Mp:         kv.mp,
					CkSessions: kv.ckSessions,
					Maker:      kv.me,
				}
				buffer := new(bytes.Buffer)
				encoder := labgob.NewEncoder(buffer)
				encoder.Encode(newSnapshot)

				kv.rf.Snapshot(applyMsg.CommandIndex, buffer.Bytes())
				DPrintf("[SKV-S][%v][%v] create snapshot up to #%v successfully", kv.gid, kv.me, applyMsg.CommandIndex)
			}
			kv.mu.Unlock()
		} else if applyMsg.SnapshotValid {
			kv.mu.Lock()
			DPrintf("[SKV-S][%v][%v] try to apply snapshot up to #%v", kv.gid, kv.me, applyMsg.SnapshotIndex)
			buffer := bytes.NewBuffer(applyMsg.Snapshot)
			decoder := labgob.NewDecoder(buffer)
			snapshot := Snapshot{}
			decoder.Decode(&snapshot)

			kv.mp = snapshot.Mp
			kv.ckSessions = snapshot.CkSessions
			kv.lastApplied = applyMsg.SnapshotIndex

			DPrintf("[SKV-S][%v][%v] apply snapshot up to #%v successfully, maker [%v]", kv.gid, kv.me, applyMsg.SnapshotIndex, snapshot.Maker)
			kv.mu.Unlock()
		}
	}
}

func (kv *ShardKV) pollConfig() {
	for !kv.killed() {
		newConfig := kv.mck.Query(-1)

		kv.mu.Lock()
		if newConfig.Num > kv.config.Num {
			args := ChangeConfigArgs{
				Id:     Local_ID, // specail ck Id for local "RPC"
				ReqNum: kv.localReqNum,
				Config: newConfig,
				OldNum: kv.config.Num, // for debug
				NewNum: newConfig.Num,
			}
			reply := ChangeConfigReply{}
			kv.localReqNum += 1

			kv.mu.Unlock()

			kv.ChangeConfig(&args, &reply)
		} else {
			kv.mu.Unlock()
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// the tester calls Kill() when a ShardKV instance won't
// be needed again. you are not required to do anything
// in Kill(), but it might be convenient to (for example)
// turn off debug output from this instance.
func (kv *ShardKV) Kill() {
	atomic.StoreInt32(&kv.dead, 1)
	kv.rf.Kill()
	DPrintf("[SKV-S][%v][%v] Killed", kv.gid, kv.me)
}

func (kv *ShardKV) killed() bool {
	z := atomic.LoadInt32(&kv.dead)
	return z == 1
}

// servers[] contains the ports of the servers in this group.
//
// me is the index of the current server in servers[].
//
// the k/v server should store snapshots through the underlying Raft
// implementation, which should call persister.SaveStateAndSnapshot() to
// atomically save the Raft state along with the snapshot.
//
// the k/v server should snapshot when Raft's saved state exceeds
// maxraftstate bytes, in order to allow Raft to garbage-collect its
// log. if maxraftstate is -1, you don't need to snapshot.
//
// gid is this group's GID, for interacting with the shardctrler.
//
// pass ctrlers[] to shardctrler.MakeClerk() so you can send
// RPCs to the shardctrler.
//
// make_end(servername) turns a server name from a
// Config.Groups[gid][i] into a labrpc.ClientEnd on which you can
// send RPCs. You'll need this to send RPCs to other groups.
//
// look at client.go for examples of how to use ctrlers[]
// and make_end() to send RPCs to the group owning a specific shard.
//
// StartServer() must return quickly, so it should start goroutines
// for any long-running work.
func StartServer(servers []*labrpc.ClientEnd, me int, persister *raft.Persister, maxraftstate int, gid int, ctrlers []*labrpc.ClientEnd, make_end func(string) *labrpc.ClientEnd) *ShardKV {
	// call labgob.Register on structures you want
	// Go's RPC library to marshall/unmarshall.
	labgob.Register(Op{})
	labgob.Register(GetArgs{})
	labgob.Register(PutAppendArgs{})
	labgob.Register(ChangeConfigArgs{})

	kv := new(ShardKV)
	kv.me = me
	kv.maxraftstate = maxraftstate
	kv.make_end = make_end
	kv.gid = gid
	kv.ctrlers = ctrlers
	kv.config = shardctrler.Config{}

	kv.persister = persister
	kv.applyCh = make(chan raft.ApplyMsg)
	kv.rf = raft.Make(servers, me, persister, kv.applyCh)

	kv.mp = make(map[string]string)
	kv.confirmMap = make(map[int]bool)
	kv.ckSessions = make(map[int64]Session)
	kv.logRecord = make(map[int]Op)

	kv.snapShotIndex = 0
	kv.lastApplied = 0
	kv.localReqNum = 1

	kv.mck = shardctrler.MakeClerk(kv.ctrlers)

	go kv.handleApplyMsg()
	go kv.pollConfig()

	return kv
}
