package coord

import (
	"net"
	"sync"
)

// ListenUnix claims a private local endpoint for its listener's lifetime.
// It refuses insecure directories and existing live listeners. Closing the
// listener removes its socket before releasing the lifetime lock.
func ListenUnix(path string) (net.Listener, error) {
	ep, err := listen(path)
	if err != nil {
		return nil, err
	}
	return &ownedListener{endpoint: ep}, nil
}

type ownedListener struct {
	*endpoint
	once sync.Once
	err  error
}

func (l *ownedListener) Accept() (net.Conn, error) { return l.listener.Accept() }
func (l *ownedListener) Addr() net.Addr            { return l.listener.Addr() }
func (l *ownedListener) Close() error {
	l.once.Do(func() { l.err = l.close() })
	return l.err
}
