package main

import (
	"os"
)

type stdioReadWriteCloser struct{} // https://stackoverflow.com/a/76697349/39974

// var _ io.ReadWriteCloser = (*stdioReadWriteCloser)(nil)

func (c stdioReadWriteCloser) Read(p []byte) (n int, err error) {
	return os.Stdin.Read(p)
}

func (c stdioReadWriteCloser) Write(p []byte) (n int, err error) {
	return os.Stdout.Write(p)
}

func (c stdioReadWriteCloser) Close() error {
	return nil
}
