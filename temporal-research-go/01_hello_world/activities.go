package hello_world

import (
	"context"
	"fmt"
)

func SayHello(ctx context.Context, name string) (string, error) {
	return fmt.Sprintf("Hello, %s!", name), nil
}
