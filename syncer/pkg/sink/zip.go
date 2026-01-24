package sink

import (
	"archive/zip"
	"os"
)

type ZipSink struct {
	f *os.File
	w *zip.Writer
}

func NewZipSink(path string) (*ZipSink, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &ZipSink{
		f: f,
		w: zip.NewWriter(f),
	}, nil
}

func (s *ZipSink) Write(path string, data []byte) error {
	f, err := s.w.Create(path)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return err
}

func (s *ZipSink) Close() error {
	if err := s.w.Close(); err != nil {
		s.f.Close()
		return err
	}
	return s.f.Close()
}
