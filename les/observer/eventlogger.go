package observer

// EventLogger ...
type EventLogger struct {
}

// NewEventLogger ...
func NewEventLogger(o *Chain, keyPrefix []byte) *EventLogger {
	return &EventLogger{}
}

// AddEvent ...
func (e *EventLogger) AddEvent(key []byte, event interface{}) {
	// get current UnixNano time
	// rlp encode event
	// get trie from e.o
	// add new entry in trie: e.keyPrefix + key + BigEndian(time) -> eventRlp
	// unlock trie
}

// NewChildLogger returns a new logger with the same observer chain
// and the new key prefix appended to the existing one
func (e *EventLogger) NewChildLogger(keyPrefix []byte) *EventLogger {
	return &EventLogger{}
}
