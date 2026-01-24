package sink

// Sink defines an interface for writing exported data to a destination.
type Sink interface {
	Write(path string, data []byte) error
	Close() error
}