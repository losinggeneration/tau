//go:build !unix

package coord

import (
	"crypto/sha256"
	"errors"
	"os"
	"sync"
	"time"
)

type launchReceipt struct {
	InstanceID string
	CWD        string
	Generation string
	TokenHash  [sha256.Size]byte
}

type receiptStore struct{ mu sync.RWMutex }

var (
	errNoReceipts          = errors.New("worker launch receipts require Unix file locks")
	errExpiredLaunchIntent = errNoReceipts
	errLegacyLaunchIntent  = errNoReceipts
	errFutureLaunchIntent  = errNoReceipts
	errStaleLaunchIntent   = errNoReceipts
	errReceiptLimit        = errNoReceipts
)

func openReceiptStore(string) (*receiptStore, error) { return nil, errNoReceipts }

func (s *receiptStore) check(string, time.Time) error { return errNoReceipts }

func (s *receiptStore) prune(time.Time) (map[string]struct{}, error) { return nil, errNoReceipts }

func (s *receiptStore) create(string, launchReceipt) (*os.File, error) { return nil, errNoReceipts }
func (s *receiptStore) read(string) (launchReceipt, error)             { return launchReceipt{}, errNoReceipts }

func (s *receiptStore) exited(string) (bool, error) { return false, errNoReceipts }
