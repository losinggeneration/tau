//go:build !linux

package coord

import "net"

func peerBuildID(net.Conn, int) string { return "" }
