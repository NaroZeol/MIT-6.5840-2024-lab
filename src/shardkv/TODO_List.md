base workflow

1. Leader detects config change
2. Leader blocks all client requests
3. Leader sends ChangeConfig PRC to each Follower
4. Leader waitting **MAJORITY** servers to ChangeConfig
5. Come back to normal state

ChangeConfig steps:

1. Go to MoveShards
2. Create ReceiveFrom table
3. Start goroutine to request Map according ReceiveFrom table
4. Update map
