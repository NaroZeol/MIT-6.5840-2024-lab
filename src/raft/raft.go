package raft

//
// this is an outline of the API that raft must expose to
// the service (or tester). see comments below for
// each of these functions for more details.
//
// rf = Make(...)
//   create a new Raft server.
// rf.Start(command interface{}) (index, term, isleader)
//   start agreement on a new log entry
// rf.GetState() (term, isLeader)
//   ask a Raft for its current term, and whether it thinks it is leader
// ApplyMsg
//   each time a new entry is committed to the log, each Raft peer
//   should send an ApplyMsg to the service (or tester)
//   in the same server.
//

import (
	"bytes"
	"io"
	"log"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"6.5840/labgob"
	"6.5840/labrpc"
)

// as each Raft peer becomes aware that successive log entries are
// committed, the peer should send an ApplyMsg to the service (or
// tester) on the same server, via the applyCh passed to Make(). set
// CommandValid to true to indicate that the ApplyMsg contains a newly
// committed log entry.
//
// in part 3D you'll want to send other kinds of messages (e.g.,
// snapshots) on the applyCh, but set CommandValid to false for these
// other uses.
type ApplyMsg struct {
	CommandValid bool
	Command      interface{}
	CommandIndex int

	// For 3D:
	SnapshotValid bool
	Snapshot      []byte
	SnapshotTerm  int
	SnapshotIndex int
}

type logEntry struct {
	Term    int
	Type    logType
	Command interface{}
}

// A Go object implementing a single Raft peer.
type Raft struct {
	mu        sync.Mutex          // Lock to protect shared access to this peer's state
	peers     []*labrpc.ClientEnd // RPC end points of all peers
	persister *Persister          // Object to hold this peer's persisted state
	me        int                 // this peer's index into peers[]
	dead      int32               // set by Kill()

	// Your data here (3A, 3B, 3C).
	// Look at the paper's Figure 2 for a description of what
	// state a Raft server must maintain.

	// Persistent state on all servers
	state       raftState
	currentTerm int
	log         []logEntry
	votedFor    *int

	// Snapshot
	snapshot      []byte
	snapshotIndex int
	snapshotTerm  int

	// Volatile state on all servers
	commitIndex int
	lastApplied int

	// Volatile state on leaders
	nextIndex  []int
	matchIndex []int

	// HeartBeat
	lastHeartBeat time.Time

	// Apply
	applyCh chan ApplyMsg

	cond sync.Cond
}

func (rf *Raft) localIndex(globalIndex int) int {
	return globalIndex - rf.snapshotIndex
}

func (rf *Raft) globalIndex(localIndex int) int {
	return localIndex + rf.snapshotIndex
}

func (rf *Raft) globalLogLen() int {
	return len(rf.log) + rf.snapshotIndex
}

// return currentTerm and whether this server
// believes it is the leader.
func (rf *Raft) GetState() (int, bool) {

	var term int
	var isleader bool
	// Your code here (3A).
	rf.mu.Lock()
	term = rf.currentTerm
	isleader = rf.state == RS_Leader
	rf.mu.Unlock()

	return term, isleader
}

// save Raft's persistent state to stable storage,
// where it can later be retrieved after a crash and restart.
// see paper's Figure 2 for a description of what should be persistent.
// before you've implemented snapshots, you should pass nil as the
// second argument to persister.Save().
// after you've implemented snapshots, pass the current snapshot
// (or nil if there's not yet a snapshot).
func (rf *Raft) persist() {
	w := new(bytes.Buffer)
	e := labgob.NewEncoder(w)

	e.Encode(rf.state)
	e.Encode(rf.currentTerm)
	e.Encode(rf.log)
	e.Encode(rf.snapshotIndex)
	e.Encode(rf.snapshotTerm)
	if rf.votedFor == nil {
		e.Encode(-1)
	} else {
		e.Encode(*rf.votedFor)
	}

	raftstate := w.Bytes()
	rf.persister.Save(raftstate, rf.snapshot)
}

// restore previously persisted state.
func (rf *Raft) readPersist(data []byte) {
	if data == nil || len(data) < 1 { // bootstrap without any state?
		return
	}

	log.Printf("[%v] try to readPersist", rf.me)

	r := bytes.NewBuffer(data)
	d := labgob.NewDecoder(r)

	var _state raftState
	var _currentTerm int
	var _log []logEntry
	var _snapshotIndex int
	var _snapshotTerm int
	var _votedFor *int

	if d.Decode(&_state) != nil ||
		d.Decode(&_currentTerm) != nil ||
		d.Decode(&_log) != nil ||
		d.Decode(&_snapshotIndex) != nil ||
		d.Decode(&_snapshotTerm) != nil {
		log.Printf("[%v] readPersist failed", rf.me)
		return
	}
	var tmp int
	if d.Decode(&tmp) != nil {
		log.Printf("[%v] readPersist failed", rf.me)
		return
	}
	if tmp == -1 {
		_votedFor = nil
	} else {
		_votedFor = &tmp
	}

	rf.state = _state
	rf.currentTerm = _currentTerm
	rf.log = _log
	rf.votedFor = _votedFor
	rf.snapshot = rf.persister.ReadSnapshot()
	rf.snapshotIndex = _snapshotIndex
	rf.snapshotTerm = _snapshotTerm

	rf.lastApplied = _snapshotIndex
	rf.commitIndex = _snapshotIndex
	log.Printf("[%v] readPersist succeed, restart in Term %v as state %v", rf.me, rf.currentTerm, rf.state)
}

// the service says it has created a snapshot that has
// all info up to and including index. this means the
// service no longer needs the log through (and including)
// that index. Raft should now trim its log as much as possible.
func (rf *Raft) Snapshot(index int, snapshot []byte) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	beforeSnapshotIndex := rf.snapshotIndex
	log.Printf("[%v] try to create snapshot from #%v to #%v", rf.me, beforeSnapshotIndex+1, index)
	rf.log = rf.log[rf.localIndex(index):]
	rf.snapshot = snapshot
	rf.snapshotIndex = index // index is a global index
	rf.snapshotTerm = rf.log[rf.localIndex(index)].Term
	rf.persist()

	rf.lastApplied = index
	rf.commitIndex = index
	log.Printf("[%v] create snapshot from #%v to #%v successfully", rf.me, beforeSnapshotIndex+1, index)
}

type RequestVoteArgs struct {
	Term         int
	CandiateId   int
	LastLogIndex int
	LastLogTerm  int
}

type RequestVoteReply struct {
	Term        int
	VoteGranted bool
}

type AppendEntriesArgs struct {
	Term         int
	LeaderId     int
	PrevLogIndex int
	PrevLogTerm  int
	Entries      []logEntry
	LeaderCommit int
}

type AppendEntriesReply struct {
	Term         int
	Success      bool
	PrevLogIndex int
	PrevLogTerm  int
	LastApplied  int
}

type InstallSnapshotArgs struct {
	Term              int
	LeaderId          int
	LastIncludedIndex int
	LastIncludedTerm  int
	Data              []byte
}

type InstallSnapshotReply struct {
	Term int
}

func (rf *Raft) RequestVote(args *RequestVoteArgs, reply *RequestVoteReply) {
	// Your code here (3A, 3B).
	rf.mu.Lock()
	defer rf.mu.Unlock()

	log.Printf("[%v] receive RequestVote from [%v] in term %v\n", rf.me, args.CandiateId, args.Term)

	// default return
	reply.Term = rf.currentTerm
	reply.VoteGranted = false

	lastLogIndex := rf.globalLogLen()
	lastLogTerm := rf.log[len(rf.log)-1].Term

	if args.Term < rf.currentTerm {
		log.Printf("[%v] refuse to vote for [%v] in term %v because of more up-to-date entry %v\n", rf.me, args.CandiateId, args.Term, rf.currentTerm)
		return
	} else if args.Term > rf.currentTerm {
		rf.currentTerm = args.Term
		rf.votedFor = nil
		rf.state = RS_Follower
		rf.persist()

		log.Printf("[%v] receive a RequestVote with higher term, change its term to %v\n", rf.me, rf.currentTerm)
	}

	isUpToDate := (args.LastLogTerm > lastLogTerm) || (args.LastLogTerm == lastLogTerm && args.LastLogIndex >= lastLogIndex)
	if !isUpToDate {
		log.Printf("[%v] refuse to vote for [%v] in term %v because of more up-to-date log entry\n", rf.me, args.CandiateId, args.Term)
	} else if rf.votedFor != nil && *rf.votedFor != args.CandiateId {
		log.Printf("[%v] refuse to vote for [%v] in term %v because it has voted for [%v]", rf.me, args.CandiateId, args.Term, *rf.votedFor)
	} else if (rf.votedFor == nil || *rf.votedFor == args.CandiateId) && isUpToDate {
		rf.currentTerm = args.Term
		rf.votedFor = &args.CandiateId
		rf.lastHeartBeat = time.Now() // According to the paper, heart beat time should reset when voting to somebody
		rf.state = RS_Follower
		rf.persist()

		reply.Term = rf.currentTerm
		reply.VoteGranted = true
		log.Printf("[%v] vote for [%v] in term %v\n", rf.me, args.CandiateId, args.Term)
		return
	} else {
		log.Fatalf("[%v] reach unreachable in Raft.RequestVote\n", rf.me)
	}
}

func (rf *Raft) AppendEntries(args *AppendEntriesArgs, reply *AppendEntriesReply) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	if len(args.Entries) != 0 {
		log.Printf("[%v] receive AppendEntries from [%v] in term %v from #%v to #%v\n", rf.me, args.LeaderId, args.Term, args.PrevLogIndex+1, args.PrevLogIndex+len(args.Entries))
	}

	// State Synchronization
	if args.Term >= rf.currentTerm {
		formerState := rf.state

		rf.currentTerm = args.Term
		rf.state = RS_Follower
		rf.persist()
		rf.lastHeartBeat = time.Now()
		if formerState != RS_Follower {
			log.Printf("[%v] turn to follower\n", rf.me)
		}

		reply.Success = true

		// Update Term
		if args.Term > rf.currentTerm {
			reply.Term = rf.currentTerm
			log.Printf("[%v] find a new leader [%v] in term %v", rf.me, args.LeaderId, args.Term)
		}
	} else if args.Term < rf.currentTerm {
		reply.Success = false
		reply.Term = rf.currentTerm
		log.Printf("[%v] refuce to accept AppendEntries from [%v] in term %v because of higher term %v", rf.me, args.LeaderId, args.Term, rf.currentTerm)
		return
	}

	updateCommitIndex := func() {
		// set new commit index
		if args.LeaderCommit > rf.commitIndex {
			minIndex := 0
			if args.LeaderCommit <= rf.globalLogLen()-1 {
				minIndex = args.LeaderCommit
			} else {
				minIndex = rf.globalLogLen() - 1
			}
			rf.commitIndex = minIndex
			log.Printf("[%v] update commitIndex to #%v", rf.me, rf.commitIndex)
		}
	}

	// Log Synchronization
	prevLogIndex := rf.globalLogLen() - 1
	prevLogTerm := rf.log[len(rf.log)-1].Term
	lastApplied := rf.lastApplied

	reply.PrevLogIndex = prevLogIndex
	reply.PrevLogTerm = prevLogTerm
	reply.LastApplied = lastApplied

	// Good case
	if prevLogIndex == args.PrevLogIndex && prevLogTerm == args.PrevLogTerm {
		rf.log = append(rf.log, args.Entries...)
		rf.persist()
		reply.Success = true

		// set new commit index
		updateCommitIndex()

		if len(args.Entries) != 0 { // ignore printing heart beat message
			log.Printf("[%v] append entries from #%v to #%v", rf.me, args.PrevLogIndex+1, rf.globalLogLen()-1)
		}
		return
	}

	// fix package from leader
	if prevLogIndex > args.PrevLogIndex && args.PrevLogIndex == rf.lastApplied {
		// prefix entries check
		i := 0
		for ; i < len(args.Entries) && rf.localIndex(i+rf.lastApplied+1) < len(rf.log); i++ {
			if args.Entries[i].Term != rf.log[rf.localIndex(i+rf.lastApplied+1)].Term {
				break
			}
		}
		if i == len(args.Entries) {
			log.Printf("[%v] refuse to use fix package because it's prefix entries", rf.me)
			log.Printf("[%v] lastApplied: #%v prevLogIndex: #%v prevLogTerm: %v commitIndex: #%v", rf.me, reply.LastApplied, reply.PrevLogIndex, reply.PrevLogTerm, rf.commitIndex)
			return
		}

		rf.log = append(rf.log[:rf.localIndex(args.PrevLogIndex+1)], args.Entries...) // left-closed and right-open interval for slice
		rf.persist()
		reply.Success = true

		// set new commit index
		updateCommitIndex()

		log.Printf("[%v] receive a fix package from [%v]", rf.me, args.LeaderId)
		log.Printf("[%v] append entries from #%v to #%v", rf.me, args.PrevLogIndex+1, rf.globalLogLen()-1)
		return
	}

	// Bad case
	if prevLogIndex != args.PrevLogIndex || prevLogTerm != args.PrevLogTerm {
		reply.Success = false
		if len(args.Entries) != 0 { //ignore printing heart beat message
			log.Printf("[%v] reject to append entries from #%v to #%v with different index #%v or term %v", rf.me, args.PrevLogIndex+1, args.PrevLogIndex+len(args.Entries), prevLogIndex, prevLogTerm)
			log.Printf("[%v] lastApplied: #%v prevLogIndex: #%v prevLogTerm: %v commitIndex: #%v", rf.me, reply.LastApplied, reply.PrevLogIndex, reply.PrevLogTerm, rf.commitIndex)
		}
		return
	}

	// unreachable, for debug
	log.Printf("args: %+v", *args)
	log.Printf("prevLogIndex: %+v, prevLogTerm: %+v, lastApplied: %+v", prevLogIndex, prevLogTerm, lastApplied)
	log.Fatalf("[%v] reach unreachable in Raft.AppendEntries", rf.me)
}

func (rf *Raft) InstallSnapshot(args *InstallSnapshotArgs, reply *InstallSnapshotReply) {
	rf.mu.Lock()

	log.Printf("[%v] receive InstallSnapshot from [%v]", rf.me, args.LeaderId)

	reply.Term = rf.currentTerm

	// check before install
	if args.Term < rf.currentTerm {
		log.Printf("[%v] refuse to install snapshot because of higher term", rf.me)
		rf.mu.Unlock()
		return
	}
	if args.LastIncludedIndex < rf.snapshotIndex {
		log.Printf("[%v] refuse to install snapshot because of more up-to-date snapshot", rf.me)
		rf.mu.Unlock()
		return
	}
	if args.LastIncludedIndex == rf.snapshotIndex {
		log.Printf("[%v] refuce to install snapshot because of same up-to-date snapshot", rf.me)
		rf.mu.Unlock()
		return
	}

	// Figure 13 step 6
	if rf.localIndex(args.LastIncludedIndex) >= 1 &&
		rf.localIndex(args.LastIncludedIndex) < len(rf.log) &&
		args.LastIncludedTerm == rf.log[rf.localIndex(args.LastIncludedIndex)].Term {
		rf.log = rf.log[rf.localIndex(args.LastIncludedIndex):]
	} else {
		rf.log = make([]logEntry, 0)
		// a fill entry to let rf.log start from index 1
		rf.log = append(rf.log, logEntry{
			Term:    args.LastIncludedTerm,
			Type:    LT_Noop,
			Command: "",
		})
	}
	if rf.lastApplied <= args.LastIncludedIndex {
		rf.lastApplied = args.LastIncludedIndex
	}
	if rf.commitIndex <= args.LastIncludedIndex {
		rf.commitIndex = args.LastIncludedIndex
	}
	rf.state = RS_Follower
	rf.snapshot = args.Data
	rf.snapshotIndex = args.LastIncludedIndex
	rf.snapshotTerm = args.LastIncludedTerm
	rf.persist()

	rf.mu.Unlock()
	// reset state machine
	rf.applyCh <- ApplyMsg{
		CommandValid:  false,
		Snapshot:      rf.snapshot,
		SnapshotValid: true,
		SnapshotIndex: rf.snapshotIndex,
		SnapshotTerm:  rf.snapshotTerm,
	}

	rf.mu.Lock()
	if rf.lastApplied <= args.LastIncludedIndex {
		rf.lastApplied = args.LastIncludedIndex
	}
	if rf.commitIndex <= args.LastIncludedIndex {
		rf.commitIndex = args.LastIncludedIndex
	}
	rf.mu.Unlock()

	log.Printf("[%v] install snapshot up to #%v successfully!", rf.me, args.LastIncludedIndex)
}

// example code to send a RequestVote RPC to a server.
// server is the index of the target server in rf.peers[].
// expects RPC arguments in args.
// fills in *reply with RPC reply, so caller should
// pass &reply.
// the types of the args and reply passed to Call() must be
// the same as the types of the arguments declared in the
// handler function (including whether they are pointers).
//
// The labrpc package simulates a lossy network, in which servers
// may be unreachable, and in which requests and replies may be lost.
// Call() sends a request and waits for a reply. If a reply arrives
// within a timeout interval, Call() returns true; otherwise
// Call() returns false. Thus Call() may not return for a while.
// A false return can be caused by a dead server, a live server that
// can't be reached, a lost request, or a lost reply.
//
// Call() is guaranteed to return (perhaps after a delay) *except* if the
// handler function on the server side does not return.  Thus there
// is no need to implement your own timeouts around Call().
//
// look at the comments in ../labrpc/labrpc.go for more details.
//
// if you're having trouble getting RPC to work, check that you've
// capitalized all field names in structs passed over RPC, and
// that the caller passes the address of the reply struct with &, not
// the struct itself.
func (rf *Raft) sendRequestVote(server int, args *RequestVoteArgs, reply *RequestVoteReply) bool {
	ok := rf.peers[server].Call("Raft.RequestVote", args, reply)
	return ok
}

func (rf *Raft) sendAppendEntries(server int, args *AppendEntriesArgs, reply *AppendEntriesReply) bool {
	ok := rf.peers[server].Call("Raft.AppendEntries", args, reply)
	return ok
}

func (rf *Raft) sendInstallSnapshot(server int, args *InstallSnapshotArgs, reply *InstallSnapshotReply) bool {
	ok := rf.peers[server].Call("Raft.InstallSnapshot", args, reply)
	return ok
}

// the service using Raft (e.g. a k/v server) wants to start
// agreement on the next command to be appended to Raft's log. if this
// server isn't the leader, returns false. otherwise start the
// agreement and return immediately. there is no guarantee that this
// command will ever be committed to the Raft log, since the leader
// may fail or lose an election. even if the Raft instance has been killed,
// this function should return gracefully.
//
// the first return value is the index that the command will appear at
// if it's ever committed. the second return value is the current
// term. the third return value is true if this server believes it is
// the leader.
func (rf *Raft) Start(command interface{}) (int, int, bool) {
	index := -1
	term := -1
	isLeader := true

	rf.mu.Lock()
	defer rf.mu.Unlock()

	if rf.state != RS_Leader {
		isLeader = false
		return index, term, isLeader
	}

	newLog := logEntry{
		Term:    rf.currentTerm,
		Type:    LT_Normal,
		Command: command,
	}
	rf.log = append(rf.log, newLog)
	rf.persist()
	rf.nextIndex[rf.me] = rf.globalLogLen()
	rf.matchIndex[rf.me] = rf.globalLogLen() - 1

	index = rf.globalLogLen() - 1
	term = rf.currentTerm
	isLeader = true
	log.Printf("[%v] leader append entry #%v to its log in term %v", rf.me, index, term)
	rf.cond.Signal()

	return index, term, isLeader
}

// the tester doesn't halt goroutines created by Raft after each test,
// but it does call the Kill() method. your code can use killed() to
// check whether Kill() has been called. the use of atomic avoids the
// need for a lock.
//
// the issue is that long-running goroutines use memory and may chew
// up CPU time, perhaps causing later tests to fail and generating
// confusing debug output. any goroutine with a long-running loop
// should call killed() to check whether it should stop.
func (rf *Raft) Kill() {
	atomic.StoreInt32(&rf.dead, 1)
	// Your code here, if desired.
}

func (rf *Raft) killed() bool {
	z := atomic.LoadInt32(&rf.dead)
	return z == 1
}

func (rf *Raft) ticker() {
	for !rf.killed() {
		rf.mu.Lock()

		// Heart beat timeout
		if rf.state != RS_Leader && time.Since(rf.lastHeartBeat) > TM_ElectionTimeout {
			rf.state = RS_Candiate
			rf.persist()

			// Start an election, if other one win the election, state will not be Candiate
			// or voting to someone that reset heartbeat time
			rf.mu.Unlock()

			cancelToken := int32(0)
			rf.lastHeartBeat = time.Now() // reset election timer

			go rf.election(&cancelToken)
			for {
				rf.mu.Lock()
				if time.Since(rf.lastHeartBeat) > TM_ElectionTimeout || atomic.LoadInt32(&cancelToken) == 1 ||
					rf.state != RS_Candiate || rf.killed() {
					atomic.StoreInt32(&cancelToken, 1)
					rf.mu.Unlock()
					break
				}
				rf.mu.Unlock()
				time.Sleep(30 * time.Millisecond)
			}
		} else {
			rf.mu.Unlock()
		}

		// pause for a random amount of time
		// milliseconds.
		ms := (rand.Int63() % int64(TM_RandomWaitingTime))
		time.Sleep(time.Duration(ms))
	}
}

func (rf *Raft) election(cancelToken *int32) {
	rf.mu.Lock()

	rf.votedFor = &rf.me // vote for self
	rf.currentTerm = rf.currentTerm + 1
	rf.persist()
	log.Printf("[%v] start an election in term %v\n", rf.me, rf.currentTerm)

	ballotCount := int32(1)
	args := RequestVoteArgs{
		Term:         rf.currentTerm,
		CandiateId:   rf.me,
		LastLogIndex: rf.globalLogLen(),
		LastLogTerm:  rf.log[len(rf.log)-1].Term,
	}

	isCancel := false
	votedForTerm := rf.currentTerm

	for i := range rf.peers {
		if i == rf.me {
			continue
		}

		go func(server int) {
			reply := RequestVoteReply{}

			// repeat sending vote request until
			ok := false
			retryCount := 0
			for !ok && atomic.LoadInt32(cancelToken) != 1 { // Retry forever
				ok = rf.sendRequestVote(server, &args, &reply)
				if retryCount++; retryCount >= MAX_RETRY_TIMES {
					log.Printf("[%v] try %v times to send RequestVote to [%v]. stop sending", rf.me, MAX_RETRY_TIMES, server)
					return
				}
			}
			if atomic.LoadInt32(cancelToken) == 1 {
				return // cancel
			}
			// log.Printf("[%v] send RequestVote to [%v] successfully\n", votedForCandiate, num)

			if !reply.VoteGranted && reply.Term > votedForTerm { // if there is a higher term, stop election
				rf.mu.Lock()

				// only stop once
				if !isCancel {
					rf.currentTerm = reply.Term
					rf.state = RS_Follower
					rf.votedFor = nil
					rf.persist()
					atomic.StoreInt32(cancelToken, 1) // cancel
					log.Printf("[%v] stop election because [%v] has higher term, turn to follower\n", rf.me, server)
					isCancel = true
				}

				rf.mu.Unlock()
			} else if reply.VoteGranted { // granted!
				atomic.AddInt32(&ballotCount, 1)
				log.Printf("[%v] get ballot from [%v]", rf.me, server)
			}
		}(i)
	}
	rf.mu.Unlock()

	for {
		ballot := atomic.LoadInt32(&ballotCount)

		if atomic.LoadInt32(cancelToken) == 1 {
			return
		}

		rf.mu.Lock()
		if rf.state == RS_Candiate && ballot >= (int32)(len(rf.peers)/2+1) { // exceed half, success
			rf.state = RS_Leader
			rf.persist()
			// Reinitialized after election
			for i := range rf.peers {
				if i == rf.me {
					continue
				}
				rf.nextIndex[i] = rf.globalLogLen()
				rf.matchIndex[i] = 0
			}
			rf.mu.Unlock()
			log.Printf("\033[31m[%v] win the election in term %v\033[0m", rf.me, votedForTerm)
			// go rf.syncEntries()
			go rf.serveAsLeader(rf.currentTerm)
			return
		}
		rf.mu.Unlock()

		time.Sleep(50 * time.Millisecond)
	}
}

func (rf *Raft) applyEntries() {
	for !rf.killed() {
		rf.mu.Lock()
		for {
			i := rf.lastApplied + 1
			if i <= rf.commitIndex && i < rf.globalLogLen() && rf.localIndex(i) > 0 && rf.log[rf.localIndex(i)].Type == LT_Normal {
				applyMsg := ApplyMsg{
					CommandValid:  true,
					Command:       rf.log[rf.localIndex(i)].Command,
					CommandIndex:  i,
					SnapshotValid: false,
				}

				// no lock to protect, fix for crash test
				// which applyCh will refuse to accept applyMsg when calling Snapshot
				// which make program choke if sending message across channel while holding the lock
				rf.mu.Unlock()
				rf.applyCh <- applyMsg

				rf.mu.Lock()
				if i == rf.lastApplied+1 { // if InstallSnapshot change the rf.lastApplied, ignore update
					rf.lastApplied = i
				}

				log.Printf("[%v] apply entry #%v to state machine", rf.me, i)
			} else {
				break
			}
		}
		rf.mu.Unlock()

		time.Sleep(30 * time.Millisecond)
	}
	log.Printf("[%v] stop applyEntries because of death", rf.me)
}

func (rf *Raft) syncEntries(cancelToken *int32) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	// return immediately if cancelToken had been set during lock wating time
	if atomic.LoadInt32(cancelToken) == 1 {
		return
	}

	// currentIndex := len(rf.log) - 1
	for i := range rf.peers {
		if i == rf.me {
			continue
		}

		// fix for snapshot
		realNextIndex := rf.nextIndex[i]
		if rf.nextIndex[i] < rf.globalIndex(1) {
			realNextIndex = rf.globalIndex(1)
		} else if rf.nextIndex[i] > rf.globalLogLen() {
			realNextIndex = rf.globalLogLen() // leader's last log entry (index start at 1)
		}

		args := AppendEntriesArgs{
			Term:         rf.currentTerm,
			LeaderId:     rf.me,
			PrevLogIndex: realNextIndex - 1,                                                 // Leader consider this follower's last log index
			PrevLogTerm:  rf.log[rf.localIndex(realNextIndex-1)].Term,                       // Leader conside this follower's last log term
			Entries:      append([]logEntry(nil), rf.log[rf.localIndex(realNextIndex):]...), // Deep copy to avoid data racing in Raft.AppendEntries()
			LeaderCommit: rf.commitIndex,
		}
		reply := AppendEntriesReply{}

		newNextIndex := rf.globalLogLen()

		go func(server int) {
			ok := false
			retryCount := 0
			for atomic.LoadInt32(cancelToken) != 1 && !ok {
				ok = rf.sendAppendEntries(server, &args, &reply)
				if retryCount++; retryCount >= MAX_RETRY_TIMES {
					log.Printf("[%v] try %v times to send AppendEntries to [%v], stop sending", rf.me, MAX_RETRY_TIMES, server)
					return
				}
			}
			if atomic.LoadInt32(cancelToken) == 1 { // is cancelled
				return
			}

			if reply.Success {
				rf.mu.Lock()
				defer rf.mu.Unlock()

				rf.nextIndex[server] = newNextIndex // apply success, update next index
				rf.matchIndex[server] = newNextIndex - 1
			} else {
				rf.handleFailedReply(server, &args, &reply, cancelToken)
			}
		}(i)
	}
}

func (rf *Raft) handleFailedReply(server int, args *AppendEntriesArgs, reply *AppendEntriesReply, cancelToken *int32) {
	rf.mu.Lock()
	// defer rf.cond.Signal()
	// Case 0: higher term, turn to follower
	if rf.currentTerm < reply.Term {
		atomic.StoreInt32(cancelToken, 1) // Cancel proactively to avoid a rare competitive situation
		log.Printf("[%v] get a reply from higher term %v, cancel leader tasks\n", rf.me, reply.Term)
		rf.mu.Unlock()
		return
	}

	// Case 1: same prevLogTerm, lower prevLogIndex
	// Case 2: different prevLogIndex, Leader has same prevLogTerm at prevLogIndex
	if rf.localIndex(reply.PrevLogIndex+1) > 0 &&
		(reply.PrevLogTerm == args.PrevLogTerm && reply.PrevLogIndex < args.PrevLogIndex ||
			reply.PrevLogIndex != args.PrevLogIndex && reply.PrevLogIndex < rf.globalLogLen() && reply.PrevLogTerm == rf.log[rf.localIndex(reply.PrevLogIndex)].Term) {

		rf.nextIndex[server] = reply.PrevLogIndex + 1
		log.Printf("[%v] {case 1,2}: set nextIndex[%v] to reply.PrevLogIndex+1: #%v", rf.me, server, reply.PrevLogIndex+1)
		rf.mu.Unlock()
		return
	}

	// Case 3: replying with a higher PrevLogIndex than len(rf.log)
	// e.g. :
	// S0: 2 2 2 2
	// S1: 2 3 3
	// S2: 2 3
	// when S1 send #3 to S0
	// Case 4: reply lower prevLogIndex, but leader doesn't have same prevLogTerm at prevLogIndex
	// e.g. :
	// S0: 2 2 2
	// S1: 2 2 3 3
	// S2: 2 2 3
	// when S1 send #4 to S0
	if rf.localIndex(reply.LastApplied+1) > 0 &&
		(reply.PrevLogIndex >= args.PrevLogIndex || // check first to avoid rf.log[rf.localIndex(reply.PrevLogIndex)] out of range
			(reply.PrevLogIndex < args.PrevLogIndex && reply.PrevLogTerm != rf.log[rf.localIndex(reply.PrevLogIndex)].Term)) {
		rf.nextIndex[server] = reply.LastApplied + 1
		log.Printf("[%v] {case3,4}: set nextIndex[%v] to reply.LastApplied+1: #%v", rf.me, server, reply.LastApplied+1)
		rf.mu.Unlock()
		return
	}

	// Case 5: entries which are needed to send to servers are only existed in snapshot
	if rf.localIndex(reply.PrevLogIndex+1) <= 0 || rf.localIndex(reply.LastApplied+1) <= 0 {
		snapshotArgs := InstallSnapshotArgs{
			Term:              rf.currentTerm,
			LeaderId:          rf.me,
			LastIncludedIndex: rf.snapshotIndex,
			LastIncludedTerm:  rf.snapshotTerm,
			Data:              rf.snapshot,
		}
		snapshotReply := InstallSnapshotReply{}

		rf.mu.Unlock()
		log.Printf("[%v] try to send InstallSnapshot to [%v]", rf.me, server)

		ok := false
		retryCount := 0
		for atomic.LoadInt32(cancelToken) != 1 && !ok {
			ok = rf.sendInstallSnapshot(server, &snapshotArgs, &snapshotReply)
			if retryCount++; retryCount >= MAX_RETRY_TIMES {
				log.Printf("[%v] try %v times to send InstallSnapshot to [%v] but failed, stop sending", rf.me, MAX_RETRY_TIMES, server)
				return
			}
		}
		if atomic.LoadInt32(cancelToken) == 1 { // is cancelled
			return
		}

		rf.mu.Lock()
		if rf.currentTerm < snapshotReply.Term {
			atomic.StoreInt32(cancelToken, 1) // Cancel proactively to avoid a rare competitive situation
			log.Printf("[%v] InstallSnapshot but get a reply from higher term %v, cancel leader tasks\n", rf.me, snapshotReply.Term)
			rf.mu.Unlock()
			return
		}
		rf.nextIndex[server] = snapshotArgs.LastIncludedIndex + 1

		log.Printf("[%v] send InstallSnapshot to [%v] successfully", rf.me, server)
		rf.mu.Unlock()
		return
	}

	// unreachable, for debug
	log.Printf("args: %+v", args)
	log.Printf("reply: %+v", reply)
	log.Fatalf("[%v] reach unreachable in Raft.syncEntries", rf.me)
}

func (rf *Raft) commitCheck(cancelToken *int32) {
	// Commit Check
	for {
		if atomic.LoadInt32(cancelToken) == 1 {
			return
		}
		rf.mu.Lock()
		N := rf.commitIndex // name from paper
		newCommitIndex := N
		for newCommitIndex < rf.globalLogLen() {
			counter := 0
			for _, index := range rf.matchIndex {
				if index >= N {
					counter += 1
				}
			}

			if counter >= len(rf.peers)/2+1 {
				newCommitIndex = N
			} else {
				break
			}

			N++
		}
		if newCommitIndex < rf.globalLogLen() && rf.log[rf.localIndex(newCommitIndex)].Term == rf.currentTerm {
			if rf.commitIndex != newCommitIndex {
				log.Printf("[%v] committed #%v", rf.me, newCommitIndex)
			}
			rf.commitIndex = newCommitIndex
		}
		rf.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
}

func (rf *Raft) heartBeat(cancelToken *int32) {
	for {
		if atomic.LoadInt32(cancelToken) == 1 {
			return
		}
		rf.cond.Signal()
		time.Sleep(TM_HeartBeatInterval)
	}
}

func (rf *Raft) serveAsLeader(term int) {
	cancelToken := int32(0)

	go rf.commitCheck(&cancelToken)
	go rf.heartBeat(&cancelToken)

	for {
		rf.mu.Lock()
		rf.cond.Wait()

		if atomic.LoadInt32(&cancelToken) == 1 || rf.killed() || rf.state != RS_Leader || rf.currentTerm != term {
			atomic.StoreInt32(&cancelToken, 1)
			log.Printf("[%v] lose it's power in term %v or dead, cancel all leader tasks", rf.me, term)
			rf.mu.Unlock()
			return
		}

		go rf.syncEntries(&cancelToken)
		rf.mu.Unlock()
	}
}

// the service or tester wants to create a Raft server. the ports
// of all the Raft servers (including this one) are in peers[]. this
// server's port is peers[me]. all the servers' peers[] arrays
// have the same order. persister is a place for this server to
// save its persistent state, and also initially holds the most
// recent saved state, if any. applyCh is a channel on which the
// tester or service expects Raft to send ApplyMsg messages.
// Make() must return quickly, so it should start goroutines
// for any long-running work.
func Make(peers []*labrpc.ClientEnd, me int,
	persister *Persister, applyCh chan ApplyMsg) *Raft {
	rf := &Raft{}
	rf.peers = peers
	rf.persister = persister
	rf.me = me
	rf.mu = sync.Mutex{}

	// Persistent state on all servers
	rf.currentTerm = 1
	rf.votedFor = nil
	rf.log = make([]logEntry, 0)
	rf.log = append(rf.log, logEntry{
		Term:    1,
		Type:    LT_Noop,
		Command: "", // no-op
	})

	// Volatile state on all servers
	rf.commitIndex = 0
	rf.lastApplied = 0

	rf.lastHeartBeat = time.Now()
	rf.applyCh = applyCh
	rf.cond = *sync.NewCond(&rf.mu)

	// initialize from state persisted before a crash
	rf.snapshot = make([]byte, 0)
	rf.readPersist(persister.ReadRaftState())

	log.SetOutput(io.Discard)
	log.SetOutput(os.Stdout)
	log.Printf("[%v] start in Term %v", rf.me, rf.currentTerm)

	rf.mu.Lock()
	go func() { // start server, use goroutine to fast return
		if len(rf.snapshot) != 0 {
			// ********VERY VERY IMPORTANT!!!!!!!!!!!!!!!!!!!!!!!!!*******
			// Make() must return quickly, so it should start goroutines
			// for any long-running work.
			// applyCh will receive ApplyMsg only after Make() is done
			// So we need to start goroutine to send ApplyMsg
			// Took me about 3 hours to find this problem
			log.Printf("[%v] try to rebuild state machine from snapshot", rf.me)
			rf.applyCh <- ApplyMsg{
				CommandValid:  false,
				SnapshotValid: true,
				Snapshot:      rf.snapshot,
				SnapshotTerm:  rf.snapshotTerm,
				SnapshotIndex: rf.snapshotIndex,
			}
			rf.commitIndex = rf.snapshotIndex
			rf.lastApplied = rf.snapshotIndex
			rf.log[0].Term = rf.snapshotTerm

			log.Printf("[%v] rebuild state machine from snapshot", rf.me)
		}

		// Volatile state on leaders
		rf.nextIndex = make([]int, len(rf.peers))
		for i := range rf.nextIndex {
			rf.nextIndex[i] = rf.globalLogLen() // initialized to leader last log index + 1 (Figure 2)
		}
		rf.matchIndex = make([]int, len(rf.peers))

		// start ticker goroutine to start elections
		go rf.ticker()
		// start applyEntries goroutine to apply committed entries
		go rf.applyEntries()

		if rf.state == RS_Leader {
			go rf.serveAsLeader(rf.currentTerm)
		}

		// // Always start as follower
		// rf.state = RS_Follower

		rf.mu.Unlock()
	}()

	return rf
}
