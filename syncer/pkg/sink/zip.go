// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

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
