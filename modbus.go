package main

import (
	"fmt"
	"io"
	"net"
	"time"
)

// Minimaler Modbus-TCP-Client (nur Function Code 0x03 = Read Holding Registers), stdlib-only.
// Reicht, um die GPS-Register eines Teltonika-Routers zu lesen.
func modbusReadHolding(host string, port, unit, startReg, count int, timeout time.Duration) ([]uint16, error) {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))

	// MBAP (7 Byte): TransID(2), Proto(2)=0, Länge(2)=6, Unit(1) + PDU: FC(1), Start(2), Anzahl(2)
	req := []byte{
		0x00, 0x01, 0x00, 0x00, 0x00, 0x06, byte(unit),
		0x03, byte(startReg >> 8), byte(startReg), byte(count >> 8), byte(count),
	}
	if _, err := conn.Write(req); err != nil {
		return nil, err
	}
	head := make([]byte, 9) // MBAP(7)+FC(1)+ByteCount(1)
	if _, err := io.ReadFull(conn, head); err != nil {
		return nil, err
	}
	if head[7] != 0x03 {
		return nil, fmt.Errorf("modbus-fehler/ausnahme (fc=0x%02x)", head[7])
	}
	bc := int(head[8])
	data := make([]byte, bc)
	if _, err := io.ReadFull(conn, data); err != nil {
		return nil, err
	}
	regs := make([]uint16, bc/2)
	for i := range regs {
		regs[i] = uint16(data[2*i])<<8 | uint16(data[2*i+1])
	}
	return regs, nil
}
