package claudeprovider

import (
	"context"
	"os"
	"os/signal"
)

func workerContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}
