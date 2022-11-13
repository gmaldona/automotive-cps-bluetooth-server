package main

/* A tcp client that acts as a middle man between ANKI Drive vehicles and the ANKI Drive SDK for Java.
 * Forms a tcp/ip connection to the SDK and uses tinygo BLE module for connecting to each ANKI Drive vehicle.
 * ANKI Drive vehicle firmware and message protocol can be found here:
 *		https://github.com/tenbergen/anki-drive-java/blob/master/Anki%20Drive%20Programming%20Guide.pdf
 *
 * Date:    November 13, 2022
 * Author:  Bastian Tenbergen & Gregory Maldonado
 * Version: 1.0

 * State University of New York College at Oswego
 */

import (
	"bytes"
	"encoding/hex"
	"fmt"
	cmap "github.com/orcaman/concurrent-map/v2"
	"gopkg.in/yaml.v3"
	"io/ioutil"
	"log"
	"net"
	"os"
	"regexp"
	"strings"
	"time"
	"tinygo.org/x/bluetooth"
)

var (
	server                  Server
	Adapter                 = bluetooth.DefaultAdapter
	ANKI_STR_SERVICE_UUID   = bluetooth.NewUUID([16]byte{0xBE, 0x15, 0xBE, 0xEF, 0x61, 0x86, 0x40, 0x7E, 0x83, 0x81, 0x0B, 0xD8, 0x9C, 0x4D, 0x8D, 0xF4})
	ANKI_STR_CHR_READ_UUID  = bluetooth.NewUUID([16]byte{0xBE, 0x15, 0xBE, 0xE0, 0x61, 0x86, 0x40, 0x7E, 0x83, 0x81, 0x0B, 0xD8, 0x9C, 0x4D, 0x8D, 0xF4})
	ANKI_STR_CHR_WRITE_UUID = bluetooth.NewUUID([16]byte{0xBE, 0x15, 0xBE, 0xE1, 0x61, 0x86, 0x40, 0x7E, 0x83, 0x81, 0x0B, 0xD8, 0x9C, 0x4D, 0x8D, 0xF4})
)

type Server struct {
	DiscoveredDevices     cmap.ConcurrentMap[string, AnkiVehicle]
	ConnectedDevices      cmap.ConcurrentMap[string, *bluetooth.Device]
	DeviceCharacteristics cmap.ConcurrentMap[string, []bluetooth.DeviceCharacteristic]
}

type AnkiVehicle struct {
	Address          string
	ManufacturerData string
	LocalName        string
	Addresser        bluetooth.Addresser
}

type ServerConf struct {
	Host string `yaml:"host"`
	Port string `yaml:"port"`
}

func main() {

	file, err := ioutil.ReadFile("serverconf.yml")
	if err != nil {
		log.Fatalln(err)
	}

	serverConf := ServerConf{}
	err = yaml.Unmarshal(file, &serverConf)
	if err != nil {
		log.Fatalf(err.Error())
	}

	server.DiscoveredDevices = cmap.New[AnkiVehicle]()
	server.ConnectedDevices = cmap.New[*bluetooth.Device]()
	server.DeviceCharacteristics = cmap.New[[]bluetooth.DeviceCharacteristic]()

	// Listen for connections on host and port
	l, err := net.Listen("tcp", serverConf.Host+":"+serverConf.Port)
	if err != nil {
		log.Fatalln(err)
	}

	// terminate server on port when disconnected
	defer func(l net.Listener) {
		err := l.Close()
		if err != nil {
		}
	}(l)
	fmt.Println("Starting Server...\nListening on " + serverConf.Host + ":" + serverConf.Port)
	for {
		// Listen for an incoming connection.
		conn, err := l.Accept()
		if err != nil {
			fmt.Println("Error accepting: ", err.Error())
			os.Exit(1)
		}
		// Handle connections in a new goroutine.
		go handleRequest(conn)
	}
}

// Handles the incoming requests from the tcp connection
func handleRequest(conn net.Conn) {

	// Keep grabbing messages from tcp connection until server termination
	for {
		// Make a buffer to hold incoming data.
		buf := make([]byte, 1024)
		// Read the incoming connection into the buffer.
		_, err := conn.Read(buf)
		if err != nil {
			log.Fatalln("Error reading:", err.Error())
		}

		// Create a goroutine for incoming msg and listen for the next msg
		go func(buf []byte) {
			// parsing msg so the payload can go to the vehicle - payload is at index [1]
			re, _ := regexp.Compile(";")
			split := re.Split(string(buf), -1)
			var set []string

			for i := range split {
				set = append(set, strings.Replace(split[i], "\n", "", -1))
			}

			address := set[0]
			var msg string

			if len(set) > 1 {
				msg = set[1]
			}

			fmt.Println("BUFF: ", string(buf))

			// Perform different actions based on the tcp msg recieved from ANKI SDK
			switch {
			// SCAN request from java
			case strings.Contains(string(buf), "SCAN"):
				fmt.Println("Scanning...")
				// call scan function to search for nearby vehicles
				server.DiscoveredDevices = scan()
				for _, device := range server.DiscoveredDevices.Items() {
					// for each found device, send a tcp msg to java saying found
					conn.Write([]byte("SCAN;" + device.Address + ";" + device.ManufacturerData + ";" + device.LocalName + "\n"))

					fmt.Println("Found " + device.Address)
					time.Sleep(500 * time.Millisecond)
				}
				// Stops scanning on java side
				conn.Write([]byte("SCAN;COMPLETED\n"))
				fmt.Println("Scanning Completed.")
				return

			//DISCONNECT request from java
			case strings.Contains(string(buf), "DISCONNECT"):

				// disconnect the vehicle with the address in the buffer
				address := string(bytes.Trim([]byte(set[1]), "\x00"))
				connectedDevice, ok := server.ConnectedDevices.Get(address)
				if !ok {
					log.Fatalln("Address: " + address)
				}
				connectedDevice.Disconnect()
				conn.Write([]byte("DISCONNECT;SUCCESS\n"))
				fmt.Println(address + " Disconnected.")

			// CONNECT request from java
			case strings.Contains(set[0], "CONNECT"):
				// ignore 0x0 fillers
				payload := bytes.Trim([]byte(set[1]), "\x00")

				device, _ := server.DiscoveredDevices.Get(string(payload))

				// connect to device
				connectedDevice, err := Adapter.Connect(device.Addresser, bluetooth.ConnectionParams{})
				if err != nil {
					log.Fatalln(err.Error())
				}

				// add device to concurrent map of devices
				server.ConnectedDevices.Set(device.Address, connectedDevice)
				fmt.Println("Connected to", device.Address)

				services, _ := connectedDevice.DiscoverServices([]bluetooth.UUID{ANKI_STR_SERVICE_UUID})
				if err != nil {
					fmt.Println("Failed to discover services")
					return
				}

				// Getting the writers and readers services
				service := services[0]
				characteristics, _ := service.DiscoverCharacteristics([]bluetooth.UUID{ANKI_STR_CHR_READ_UUID, ANKI_STR_CHR_WRITE_UUID})
				server.DeviceCharacteristics.Set(device.Address, characteristics)

				readService := characteristics[1]

				// Each time the vehicle sends a msg through bluetooth, the event is triggered
				readService.EnableNotifications(func(value []byte) {
					encodedBytes := hex.EncodeToString(value)
					// Send the vehicle respond back to java
					conn.Write([]byte(device.Address + ";" + encodedBytes + "\n"))
					fmt.Println("RECEIVED: [" + device.Address + ";" + encodedBytes + "]")
				})

				// terminate connection request to java
				conn.Write([]byte("CONNECT;SUCCESS\n"))
				fmt.Println("CONNECT COMPLETED")
				return

			/* Any other request is assumed to be a command given to the car. Each byte in the buffer represents an action that is
			outlined in https://github.com/tenbergen/anki-drive-java/blob/master/Anki%20Drive%20Programming%20Guide.pdf
			*/
			default:
				if len(set) == 2 {
					// Get the writer characteristic
					characteristics, _ := server.DeviceCharacteristics.Get(address)
					writeService := characteristics[0]
					payload, _ := hex.DecodeString(msg)

					// write payload to anki vehicle
					_, err := writeService.WriteWithoutResponse(payload)
					if err != nil {
						fmt.Println(err)
						return
					}

					fmt.Println("SENDING: [" + strings.Replace(string(buf), "\n", "", -1) + "]")
				}
			}
		}(buf)
	}
}

// function for scanning nearby vehicles returns a map of addresses to vehicles
func scan() cmap.ConcurrentMap[string, AnkiVehicle] {
	m := cmap.New[AnkiVehicle]()

	channel := make(chan string, 1)
	// func that is wrapped, so it can time out in some number of seconds
	go func() {
		must("enable BLE stack", Adapter.Enable())

		err := Adapter.Scan(func(adapter *bluetooth.Adapter, device bluetooth.ScanResult) {
			// only scan for devices that contain "Drive" for anki drive
			if strings.Contains(device.LocalName(), "Drive") {
				if !m.Has(device.Address.String()) {
					var manufacturerData = ""
					for _, data := range device.ManufacturerData() {
						manufacturerData = "beef" + hex.EncodeToString(data)
					}
					var localname = "10603001202020204472697665"
					// ANKI device properties
					m.Set(strings.Replace(device.Address.String(), "-", "", -1), AnkiVehicle{
						Address:          strings.Replace(device.Address.String(), "-", "", -1),
						ManufacturerData: manufacturerData,
						LocalName:        localname,
						Addresser:        device.Address,
					})
				}
			}
		})
		must("start scan", err)
		must("enable BLE stack", Adapter.StopScan())

		channel <- "finished scanning"
	}()

	// timeout scan
	select {
	case <-channel:
		break
	case <-time.After(5 * time.Second):
		break
	}

	return m
}

func must(action string, err error) {
	if err != nil {
		panic("failed to " + action + ": " + err.Error())
	}
}
