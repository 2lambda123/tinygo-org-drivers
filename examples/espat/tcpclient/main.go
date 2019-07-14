// This is a sensor station that uses a ESP8266 or ESP32 running on the device UART1.
// It creates a UDP connection you can use to get info to/from your computer via the microcontroller.
//
// In other words:
// Your computer <--> UART0 <--> MCU <--> UART1 <--> ESP8266
//
package main

import (
	"machine"
	"time"

	"tinygo.org/x/drivers/espat"
	"tinygo.org/x/drivers/espat/net"
)

// access point info
const ssid = "YOURSSID"
const pass = "YOURPASS"

// IP address of the server aka "hub". Replace with your own info.
const serverIP = "0.0.0.0"

// change these to connect to a different UART or pins for the ESP8266/ESP32
var (
	uart = machine.UART1
	tx   = machine.PA22
	rx   = machine.PA23

	adaptor *espat.Device
)

func main() {
	uart.Configure(machine.UARTConfig{TX: tx, RX: rx})

	// Init esp8266/esp32
	adaptor = espat.New(uart)
	adaptor.Configure()

	// first check if connected
	if adaptor.Connected() {
		println("Connected to wifi adaptor.")
		adaptor.Echo(false)

		connectToAP()
	} else {
		println("Unable to connect to wifi adaptor.")
		return
	}

	// now make TCP connection
	ip := net.ParseIP(serverIP)
	raddr := &net.TCPAddr{IP: ip, Port: 8080}
	laddr := &net.TCPAddr{Port: 8080}

	println("Dialing TCP connection...")
	conn, _ := net.DialTCP("tcp", laddr, raddr)

	for {
		// send data
		println("Sending data...")
		conn.Write([]byte("hello\r\n"))

		time.Sleep(1000 * time.Millisecond)
	}

	// Right now this code is never reached. Need a way to trigger it...
	println("Disconnecting TCP...")
	conn.Close()
	println("Done.")
}

// connect to access point
func connectToAP() {
	println("Connecting to wifi network...")
	adaptor.SetWifiMode(espat.WifiModeClient)
	adaptor.ConnectToAP(ssid, pass, 10)
	println("Connected.")
	println(adaptor.GetClientIP())
}
