package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
)

func ReadVarInt(conn net.Conn) (int, error) {
	var value int
	var position int
	buffer := make([]byte, 1)

	for {
		_, err := io.ReadFull(conn, buffer)
		if err != nil {
			return 0, err
		}
		currentByte := buffer[0]

		value |= int(currentByte&0x7F) << position

		if (currentByte & 0x80) == 0 {
			break
		}
		position += 7
		if position >= 32 {
			return 0, fmt.Errorf("VarInt is too big")
		}
	}
	return value, nil
}

func ReadString(conn net.Conn) (string, error) {
	length, err := ReadVarInt(conn)
	if err != nil {
		return "", err
	}

	buffer := make([]byte, length)

	_, err = io.ReadFull(conn, buffer)
	if err != nil {
		return "", err
	}

	return string(buffer), nil
}

func WriteVarInt(conn net.Conn, value int) error {
	for {
		temp := byte(value & 0x7F)
		value >>= 7
		if value != 0 {
			temp |= 0x80
		}
		_, err := conn.Write([]byte{temp})
		if err != nil {
			return err
		}
		if value == 0 {
			break
		}
	}
	return nil
}

func WriteVarIntToBuffer(buf *bytes.Buffer, value int) {
	for {
		temp := byte(value & 0x7F)
		value >>= 7
		if value != 0 {
			temp |= 0x80
		}
		buf.WriteByte(temp)
		if value == 0 {
			break
		}
	}
}

func WriteStatusResponse(conn net.Conn, json string) {
	var packetData bytes.Buffer
	WriteVarIntToBuffer(&packetData, 0x00)

	jsonBytes := []byte(json)
	WriteVarIntToBuffer(&packetData, len(jsonBytes))
	packetData.Write(jsonBytes)

	WriteVarInt(conn, packetData.Len())
	conn.Write(packetData.Bytes())
}

func WritePongPacket(conn net.Conn, payload []byte) {
	var packetData bytes.Buffer
	WriteVarIntToBuffer(&packetData, 0x01)
	packetData.Write(payload)

	WriteVarInt(conn, packetData.Len())
	conn.Write(packetData.Bytes())
}

func WriteDisconnectPacket(conn net.Conn, jsonReason string) {
	var packetData bytes.Buffer
	WriteVarIntToBuffer(&packetData, 0x00)

	jsonBytes := []byte(jsonReason)
	WriteVarIntToBuffer(&packetData, len(jsonBytes))
	packetData.Write(jsonBytes)

	WriteVarInt(conn, packetData.Len())
	conn.Write(packetData.Bytes())
}
