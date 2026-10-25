// Package intercept provides synchronous host callbacks for Sentry syscalls.
// It contains no filesystem policy. The runtime uses Message to carry callback
// operations across its private platform device socket.
package intercept

type Message struct {
	UID, GID uint32
	Groups   []uint32
	Op       string    `json:"op"`
	Name     string    `json:"name,omitempty"`
	Args     [6]uint64 `json:"args,omitempty"`
	First    bool      `json:"first,omitempty"`
	Address  uint64    `json:"address,omitempty"`
	Length   uint64    `json:"length,omitempty"`
	Size     uint64    `json:"size,omitempty"`
	Data     []byte    `json:"data,omitempty"`
	Path     string    `json:"path,omitempty"`
	Link     *string   `json:"link,omitempty"`
	Mode     uint32    `json:"mode,omitempty"`
	DirFD    int32     `json:"dirfd,omitempty"`
	Follow   bool      `json:"follow,omitempty"`
	Errno    uint32    `json:"errno,omitempty"`
	Error    string    `json:"error,omitempty"`
	Result   *Result   `json:"result,omitempty"`
	Ranges   []Range   `json:"ranges,omitempty"`
}

// Result replaces the ordinary syscall result when a callback supplies it.
type Result struct {
	Value uint64
	Errno uint32
}

// Range identifies pages in a donated FD, relative to the guest allocation.
type Range struct {
	FD     int
	Offset uint64
	Start  uint64
	Length uint64
}
