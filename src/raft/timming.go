package raft

import "time"

const (
	TM_HeartBeatInterval time.Duration = 100 * time.Millisecond // heart beat interval
	TM_ElectionTimeout   time.Duration = 300 * time.Millisecond // election timeout
	TM_RandomWaitingTime time.Duration = 800 * time.Millisecond // randomization, wait for 0~TM_RandomWaitingTime
	MAX_RETRY_TIMES      int           = 3                      // Max times a server can try during sending a RPC
)
