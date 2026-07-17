package main

import (
	"context"
	"os"
)

func main() {
	code, err := dispatch(context.Background(), os.Args[1:])
	if err != nil {
		_, _ = os.Stderr.WriteString(err.Error() + "\n")
	}
	os.Exit(code)
}
