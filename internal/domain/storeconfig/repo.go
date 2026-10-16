package storeconfig

import "context"

// Repository persists the most recent store_config the agent received from
// main/'s SSE handshake. Singleton row — every save overwrites the previous
// payload. Reads can return an empty StoreContext when the agent hasn't
// connected to main/ yet.
type Repository interface {
	Get(ctx context.Context) (*StoreContext, error)
	Save(ctx context.Context, sc StoreContext) error
}
