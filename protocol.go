package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
)

// ReadVarInt: decodes a Minecraft VarInt from the client
func ReadVarInt(conn net.Conn) (int, error) {
	var value int
	var position int
	buffer := make([]byte, 1) // read varint 1 byte at a time

	for {
		_, err := io.ReadFull(conn, buffer)
		if err != nil {
			return 0, err
		}
		currentByte := buffer[0]

		// use AND to isolate the payload bytes, shift them to the correct pos and OR them into value
		value |= int(currentByte&0x7F) << position

		// use AND to see if top bit is an stop bit (0)
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

// ReadString: decodes a Minecraft String from the client (length-prefixed with a VarInt)
func ReadString(conn net.Conn) (string, error) {
	length, err := ReadVarInt(conn)
	if err != nil {
		return "", err
	}

	// Prevent OOM DoS attacks by capping length to the protocol's 32767 character max limit
	if length < 0 || length > 32767*4 {
		return "", fmt.Errorf("string length exceeds maximum allowed")
	}

	buffer := make([]byte, length) // lenght = varint

	_, err = io.ReadFull(conn, buffer)
	if err != nil {
		return "", err
	}

	return string(buffer), nil
}

// WriteVarInt: encodes an integer to the Minecraft VarInt format directly to the socket
func WriteVarInt(conn net.Conn, value int) error {
	for {
		temp := byte(value & 0x7F) // isolate the lowest 7 bits
		value >>= 7                // shift the value to get the next 7 bits in the next iteration
		if value != 0 {
			temp |= 0x80 // set the stop bit if there are more bytes to write
		}
		_, err := conn.Write([]byte{temp}) // write the byte to the socket
		if err != nil {
			return err
		}
		if value == 0 {
			break // nothing more to write
		}
	}
	return nil
}

func WriteVarIntToBuffer(buf *bytes.Buffer, value int) {
	for {
		temp := byte(value & 0x7F) // isolate the lowest 7 bits
		value >>= 7                // shift the value to get the next 7 bits in the next iteration
		if value != 0 {
			temp |= 0x80 // set the stop bit if there are more bytes to write
		}
		buf.WriteByte(temp)
		if value == 0 {
			break
		}
	}
}

// WriteStatusResponse: builds and sends the MOTD JSON packet to the client
func WriteStatusResponse(conn net.Conn, json string) {
	var packetData bytes.Buffer
	WriteVarIntToBuffer(&packetData, 0x00) // 0x00 = status response packet id

	// write json as string w/ length varint
	jsonBytes := []byte(json)
	WriteVarIntToBuffer(&packetData, len(jsonBytes))
	packetData.Write(jsonBytes)

	// Combine length and payload into a single buffer to avoid TCP fragmentation delay
	var finalPacket bytes.Buffer
	WriteVarIntToBuffer(&finalPacket, packetData.Len())
	finalPacket.Write(packetData.Bytes())

	conn.Write(finalPacket.Bytes()) // send complete packet at once
}

// WritePongPacket: sends the exact timestamp back to the client to measure latency
func WritePongPacket(conn net.Conn, payload []byte) {
	var packetData bytes.Buffer
	WriteVarIntToBuffer(&packetData, 0x01) // 0x01 = pong packet id
	packetData.Write(payload)              // Append the 8 bytes of time data

	var finalPacket bytes.Buffer
	WriteVarIntToBuffer(&finalPacket, packetData.Len())
	finalPacket.Write(packetData.Bytes())

	conn.Write(finalPacket.Bytes()) // send complete packet at once
}

// WriteDisconnectPacket: sends a kick message to the player during login
func WriteDisconnectPacket(conn net.Conn, jsonReason string) {
	var packetData bytes.Buffer
	WriteVarIntToBuffer(&packetData, 0x00) // 0x00 = disconnect packet id in login

	// write the JSON string (length varint + payload string)
	jsonBytes := []byte(jsonReason)
	WriteVarIntToBuffer(&packetData, len(jsonBytes))
	packetData.Write(jsonBytes)

	var finalPacket bytes.Buffer
	WriteVarIntToBuffer(&finalPacket, packetData.Len())
	finalPacket.Write(packetData.Bytes())

	conn.Write(finalPacket.Bytes()) // send complete packet at once
}
