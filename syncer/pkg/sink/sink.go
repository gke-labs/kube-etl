package sink

type Sink interface {
	Write(path string, data []byte) error
	Close() error
}
