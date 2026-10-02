package chat

import (
	"encoding/json"
	"time"
)

// callTimeout bounds one Herdr method call made on a chat message's behalf.
// Short, because somebody is waiting in a chat window: a prompt that has not
// been accepted in this long is better reported than waited on.
const callTimeout = 15 * time.Second

func decode(line []byte, v any) error { return json.Unmarshal(line, v) }
