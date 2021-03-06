// +build darwin

package tunnet

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"sync"
	"unsafe"

	"github.com/unixpickle/essentials"

	"golang.org/x/sys/unix"
)

const (
	utunOptIfname = 2
	utunControl   = "com.apple.net.utun_control"
)

// MakeTunnel creates a new tunnel interface.
func MakeTunnel() (Tunnel, error) {
	tun, err := openUtunSocket()
	err = essentials.AddCtx("make tunnel", err)
	return tun, err
}

type utunSocket struct {
	fd   int
	name string

	refLock  sync.Mutex
	refCount int
	closed   bool
}

func openUtunSocket() (res *utunSocket, err error) {
	fd, err := unix.Socket(pfSystem, unix.SOCK_DGRAM, sysprotoControl)
	if err != nil {
		return nil, err
	}
	socket := &utunSocket{fd: fd}

	defer func() {
		if err != nil {
			unix.Close(socket.fd)
		}
	}()

	controlID, err := socket.getControlID()
	if err != nil {
		return nil, err
	}

	if err := socket.connectToControl(controlID); err != nil {
		return nil, err
	}

	nameData := make([]byte, 32)
	nameLen := uintptr(len(nameData))
	_, _, sysErr := unix.Syscall6(unix.SYS_GETSOCKOPT, uintptr(socket.fd), sysprotoControl,
		utunOptIfname, uintptr(unsafe.Pointer(&nameData[0])), uintptr(unsafe.Pointer(&nameLen)), 0)
	if sysErr != 0 {
		return nil, sysErr
	}

	socket.name = string(nameData[:nameLen-1])

	return socket, nil
}

func (u *utunSocket) Name() string {
	return u.name
}

func (u *utunSocket) ReadPacket() (packet []byte, err error) {
	defer essentials.AddCtxTo("read packet", &err)
	if err := u.retain(); err != nil {
		return nil, err
	}
	defer u.release()
	packet = make([]byte, 65536)
	for {
		amount, err := unix.Read(u.fd, packet)
		if err == nil {
			return packet[4:amount], nil
		} else if err == unix.EINTR {
			continue
		} else {
			return nil, err
		}
	}
}

func (u *utunSocket) WritePacket(buffer []byte) (err error) {
	defer essentials.AddCtxTo("write packet", &err)
	if err := u.retain(); err != nil {
		return err
	}
	defer u.release()
	_, err = unix.Write(u.fd, append([]byte{0, 0, 0, 2}, buffer...))
	return err
}

func (u *utunSocket) MTU() (mtu int, err error) {
	defer essentials.AddCtxTo("get MTU", &err)
	buf := make([]byte, 4)
	if err := u.ifreqIOCTL(ioctlSIOCGIFMTU, buf); err != nil {
		return 0, err
	}
	var value uint32
	binary.Read(bytes.NewReader(buf), systemByteOrder, &value)
	return int(value), nil
}

func (u *utunSocket) SetMTU(mtu int) (err error) {
	defer essentials.AddCtxTo("set MTU", &err)
	var buf bytes.Buffer
	binary.Write(&buf, systemByteOrder, uint32(mtu))
	return u.ifreqIOCTL(ioctlSIOCSIFMTU, buf.Bytes())
}

func (u *utunSocket) Addresses() (local, dest net.IP, mask net.IPMask, err error) {
	defer essentials.AddCtxTo("get addresses", &err)

	sockaddrOut := packSockaddr4(net.IPv4zero, 0)

	ips := []net.IP{}
	ioctls := []int{ioctlSIOCGIFADDR, ioctlSIOCGIFDSTADDR, ioctlSIOCGIFNETMASK}
	for _, ioctl := range ioctls {
		if err := u.ifreqIOCTL(ioctl, sockaddrOut); err != nil {
			return nil, nil, nil, err
		}
		ip, _ := unpackSockaddr4(sockaddrOut)
		ips = append(ips, ip)
	}
	return ips[0], ips[1], net.IPMask(ips[2]), nil
}

func (u *utunSocket) SetAddresses(local, dest net.IP, mask net.IPMask) (err error) {
	defer essentials.AddCtxTo("set addresses", &err)

	if local.To4() == nil || dest.To4() == nil || len(mask) != 4 {
		return errors.New("only IPv4 is supported")
	}

	u.ifreqIOCTL(ioctlSIOCDIFADDR, make([]byte, 16*3))

	var sockaddrs bytes.Buffer
	for _, ip := range []net.IP{local, dest, net.IP(mask)} {
		sockaddrs.Write(packSockaddr4(ip, 0))
	}
	if err := u.ifreqIOCTL(ioctlSIOCAIFADDR, sockaddrs.Bytes()); err != nil {
		return err
	}
	return nil
}

func (u *utunSocket) Close() (err error) {
	defer essentials.AddCtxTo("close", &err)
	if err := u.retain(); err != nil {
		return err
	}
	defer u.release()
	return unix.Shutdown(u.fd, unix.SHUT_RDWR)
}

func (u *utunSocket) getControlID() ([]byte, error) {
	// struct ctl_info
	ctlInfo := make([]byte, 128)
	copy(ctlInfo[4:], []byte(utunControl))
	_, _, sysErr := unix.Syscall(unix.SYS_IOCTL, uintptr(u.fd), uintptr(ioctlCTLIOCGINFO),
		uintptr(unsafe.Pointer(&ctlInfo[0])))
	if sysErr != 0 {
		return nil, sysErr
	}
	return ctlInfo[:4], nil
}

func (u *utunSocket) connectToControl(controlID []byte) error {
	// struct sockaddr_ctl
	addrData := make([]byte, 32)
	addrData[0] = 32
	addrData[1] = unix.AF_SYSTEM
	addrData[2] = afSysControl
	copy(addrData[4:], controlID)
	_, _, sysErr := unix.Syscall(unix.SYS_CONNECT, uintptr(u.fd),
		uintptr(unsafe.Pointer(&addrData[0])), uintptr(32))
	if sysErr != 0 {
		return sysErr
	}
	return nil
}

func (u *utunSocket) ifreqIOCTL(ioctl int, reqData []byte) error {
	sock, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer unix.Close(sock)

	var ifreq []byte
	if len(reqData) > 16 {
		ifreq = make([]byte, 16+len(reqData))
	} else {
		ifreq = make([]byte, 32)
	}
	copy(ifreq[:16], []byte(u.Name()))
	copy(ifreq[16:], reqData)
	_, _, sysErr := unix.Syscall(unix.SYS_IOCTL, uintptr(sock), uintptr(ioctl),
		uintptr(unsafe.Pointer(&ifreq[0])))
	copy(reqData, ifreq[16:])
	if sysErr == 0 {
		return nil
	}
	return sysErr
}

func (u *utunSocket) retain() error {
	u.refLock.Lock()
	defer u.refLock.Unlock()
	if u.closed {
		return os.ErrClosed
	}
	u.refCount += 1
	return nil
}

func (u *utunSocket) release() {
	u.refLock.Lock()
	defer u.refLock.Unlock()
	u.refCount -= 1
	if u.closed && u.refCount == 0 {
		unix.Close(u.fd)
	}
}
