package cmd

import (
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/cskr/pubsub"
	"github.com/dh1tw/gorigctl/comms"
	"github.com/dh1tw/gorigctl/events"
	"github.com/dh1tw/gorigctl/ping"
	"github.com/dh1tw/gorigctl/server"
	"github.com/dh1tw/gorigctl/utils"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	hl "github.com/dh1tw/goHamlib"
	sbLog "github.com/dh1tw/gorigctl/sb_log"
	sbStatus "github.com/dh1tw/gorigctl/sb_status"

	_ "net/http/pprof"
)

// serverMqttCmd represents the mqtt command
var serverMqttCmd = &cobra.Command{
	Use:   "mqtt",
	Short: "MQTT server which makes a local radio available on the network",
	Long: `MQTT server which makes a local radio available on the network
		
The MQTT Topics follow the Shackbus convention and must match on the
Server and the Client.

The parameters in "<>" can be set through flags or in the config file:
<station>/radios/<radio>/cat

	`,
	Run: mqttRadioServer,
}

func init() {
	serverCmd.AddCommand(serverMqttCmd)
	serverMqttCmd.Flags().StringP("broker-url", "u", "test.mosquitto.org", "MQTT Broker URL")
	serverMqttCmd.Flags().IntP("broker-port", "p", 1883, "MQTT Broker Port")
	serverMqttCmd.Flags().StringP("username", "U", "", "MQTT Username")
	serverMqttCmd.Flags().StringP("password", "P", "", "MQTT Password")
	serverMqttCmd.Flags().StringP("client-id", "C", "gorigctl-svr", "MQTT ClientID")
	serverMqttCmd.Flags().StringP("station", "X", "mystation", "Your station callsign")
	serverMqttCmd.Flags().StringP("radio", "Y", "myradio", "Radio ID")
	serverMqttCmd.Flags().DurationP("polling-interval", "t", time.Duration(time.Millisecond*100), "Timer for polling the rig's meter values [ms] (0 = disabled)")
	serverMqttCmd.Flags().DurationP("sync-interval", "k", time.Duration(time.Second*3), "Timer for syncing all values with the rig [s] (0 = disabled)")
	serverMqttCmd.Flags().IntP("rig-model", "m", 1, "Hamlib Rig Model ID")
	serverMqttCmd.Flags().IntP("baudrate", "b", 38400, "Baudrate")
	serverMqttCmd.Flags().StringP("portname", "o", "/dev/mhux/cat", "Portname / Device path")
	serverMqttCmd.Flags().IntP("databits", "d", 8, "Databits")
	serverMqttCmd.Flags().IntP("stopbits", "s", 1, "Stopbits")
	serverMqttCmd.Flags().StringP("parity", "r", "none", "Parity")
	serverMqttCmd.Flags().StringP("handshake", "a", "none", "Handshake")
	serverMqttCmd.Flags().IntP("hl-debug-level", "D", 0, "Hamlib Debug Level (0=ERROR,..., 5=TRACE)")
}

func mqttRadioServer(cmd *cobra.Command, args []string) {

	// If a config file is found, read it in.
	if err := viper.ReadInConfig(); err == nil {
		fmt.Println("Using config file:", viper.ConfigFileUsed())
	}

	// bind the pflags to viper settings
	viper.BindPFlag("mqtt.username", cmd.Flags().Lookup("username"))
	viper.BindPFlag("mqtt.password", cmd.Flags().Lookup("password"))
	viper.BindPFlag("mqtt.client-id", cmd.Flags().Lookup("client-id"))
	viper.BindPFlag("mqtt.broker-url", cmd.Flags().Lookup("broker-url"))
	viper.BindPFlag("mqtt.broker-port", cmd.Flags().Lookup("broker-port"))
	viper.BindPFlag("mqtt.station", cmd.Flags().Lookup("station"))
	viper.BindPFlag("mqtt.radio", cmd.Flags().Lookup("radio"))
	viper.BindPFlag("radio.rig-model", cmd.Flags().Lookup("rig-model"))
	viper.BindPFlag("radio.baudrate", cmd.Flags().Lookup("baudrate"))
	viper.BindPFlag("radio.portname", cmd.Flags().Lookup("portname"))
	viper.BindPFlag("radio.databits", cmd.Flags().Lookup("databits"))
	viper.BindPFlag("radio.stopbits", cmd.Flags().Lookup("stopbits"))
	viper.BindPFlag("radio.parity", cmd.Flags().Lookup("parity"))
	viper.BindPFlag("radio.handshake", cmd.Flags().Lookup("handshake"))
	viper.BindPFlag("radio.polling-interval", cmd.Flags().Lookup("polling-interval"))
	viper.BindPFlag("radio.sync-interval", cmd.Flags().Lookup("sync-interval"))
	viper.BindPFlag("radio.hl-debug-level", cmd.Flags().Lookup("hl-debug-level"))

	// profiling server can be enabled through a hidden pflag
	// go func() {
	// 	log.Println(http.ListenAndServe("localhost:6060", nil))
	// }()

	// viper settings need to be copied in local variables
	// since viper lookups allocate of each lookup a copy
	// and are quite inperformant

	mqttBrokerURL := viper.GetString("mqtt.broker-url")
	mqttBrokerPort := viper.GetInt("mqtt.broker-port")
	mqttUsername := viper.GetString("mqtt.username")
	mqttPassword := viper.GetString("mqtt.password")
	mqttClientID := viper.GetString("mqtt.client-id")

	if mqttClientID == "gorigctl-svr" {
		mqttClientID = mqttClientID + "-" + utils.RandStringRunes(5)
	}

	hlDebugLevel := viper.GetInt("radio.hl-debug-level")

	baseTopic := viper.GetString("mqtt.station") +
		"/radios/" + viper.GetString("mqtt.radio") +
		"/cat"

	serverCatRequestTopic := baseTopic + "/setstate"
	serverStatusTopic := baseTopic + "/status"
	serverPingTopic := baseTopic + "/ping"
	logTopic := baseTopic + "/log"

	// tx topics
	serverCatResponseTopic := baseTopic + "/state"
	serverCapsTopic := baseTopic + "/caps"
	serverPongTopic := baseTopic + "/pong"
	serverCapsReqTopic := baseTopic + "/capsreq"

	mqttRxTopics := []string{serverCatRequestTopic, serverPingTopic, serverCapsReqTopic}

	toWireCh := make(chan comms.IOMsg, 20)
	// toSerializeCatDataCh := make(chan comms.IOMsg, 20)
	toDeserializeCatRequestCh := make(chan []byte, 10)
	toDeserializePingRequestCh := make(chan []byte, 10)
	toDeserializeCapsReqCh := make(chan []byte, 10)

	// Event PubSub
	evPS := pubsub.New(100)

	// WaitGroup to coordinate a graceful shutdown
	var wg sync.WaitGroup

	// mqtt Last Will Message
	binaryWillMsg, err := createLastWillMsg()
	if err != nil {
		fmt.Println(err)
	}

	lastWill := comms.LastWill{
		Topic:  serverStatusTopic,
		Data:   binaryWillMsg,
		Qos:    0,
		Retain: true,
	}

	appLogger := utils.NewStdLogger("", log.Ltime)
	radioLogger := utils.NewChLogger(evPS, events.RadioLog, "")

	mqttSettings := comms.MqttSettings{
		WaitGroup:  &wg,
		Transport:  "tcp",
		BrokerURL:  mqttBrokerURL,
		BrokerPort: mqttBrokerPort,
		ClientID:   mqttClientID,
		Username:   mqttUsername,
		Password:   mqttPassword,
		Topics:     mqttRxTopics,
		ToDeserializeCatRequestCh:  toDeserializeCatRequestCh,
		ToDeserializePingRequestCh: toDeserializePingRequestCh,
		ToDeserializeCapsReqCh:     toDeserializeCapsReqCh,
		ToWire:                     toWireCh,
		Events:                     evPS,
		LastWill:                   &lastWill,
		Logger:                     appLogger,
	}

	pongSettings := ping.Settings{
		PongCh:    toDeserializePingRequestCh,
		ToWireCh:  toWireCh,
		PongTopic: serverPongTopic,
		WaitGroup: &wg,
		Events:    evPS,
	}

	rigModel := viper.GetInt("radio.rig-model")

	port := hl.Port{}
	port.Baudrate = viper.GetInt("radio.baudrate")
	port.Databits = viper.GetInt("radio.databits")
	port.Stopbits = viper.GetInt("radio.stopbits")
	port.Portname = viper.GetString("radio.portname")
	port.RigPortType = hl.RIG_PORT_SERIAL
	switch viper.GetString("radio.parity") {
	case "none":
		port.Parity = hl.N
	case "even":
		port.Parity = hl.E
	case "odd":
		port.Parity = hl.O
	default:
		port.Parity = hl.N
	}

	switch viper.GetString("radio.handshake") {
	case "none":
		port.Handshake = hl.NO_HANDSHAKE
	case "RTSCTS":
		port.Handshake = hl.RTSCTS_HANDSHAKE
	default:
		port.Handshake = hl.NO_HANDSHAKE
	}

	pollingInterval := viper.GetDuration("radio.polling-interval")
	syncInterval := viper.GetDuration("radio.sync-interval")

	radioSettings := server.RadioSettings{
		RigModel:         rigModel,
		Port:             port,
		HlDebugLevel:     hlDebugLevel,
		CatRequestCh:     toDeserializeCatRequestCh,
		CapsReqCh:        toDeserializeCapsReqCh,
		ToWireCh:         toWireCh,
		CatResponseTopic: serverCatResponseTopic,
		CapsTopic:        serverCapsTopic,
		WaitGroup:        &wg,
		Events:           evPS,
		PollingInterval:  pollingInterval,
		SyncInterval:     syncInterval,
		RadioLogger:      radioLogger,
		AppLogger:        appLogger,
	}

	wg.Add(4) //MQTT + Ping + Radio + Events

	connectionStatusCh := evPS.Sub(events.MqttConnStatus)
	shutdownCh := evPS.Sub(events.Shutdown)
	prepareShutdownCh := evPS.Sub(events.PrepareShutdown)

	appLoggingCh := evPS.Sub(events.AppLog)
	radioLoggingCh := evPS.Sub(events.RadioLog)

	go events.WatchSystemEvents(evPS, &wg)
	go comms.MqttClient(mqttSettings)
	go ping.EchoPing(pongSettings)

	time.Sleep(time.Millisecond * 500)
	go server.StartRadioServer(radioSettings)

	status := serverStatus{}
	status.statusTopic = serverStatusTopic
	status.logTopic = logTopic
	status.toWireCh = toWireCh

	for {
		select {
		case <-prepareShutdownCh:

			// wait for 200 ms so that CAPS and STATE
			// can send their final message
			time.Sleep(time.Microsecond * 200)
			// publish that the server is going offline
			status.online = false
			if err := status.sendUpdate(); err != nil {
				fmt.Println(err)
			}
			time.Sleep(time.Millisecond * 100)
			// inform the other goroutines to shut down
			evPS.Pub(true, events.Shutdown)

		// shutdown the application gracefully
		case <-shutdownCh:
			//force exit after 1 sec
			exitTimeout := time.NewTimer(time.Second)
			go func() {
				<-exitTimeout.C
				fmt.Println("quitting forcefully")
				os.Exit(0)
			}()

			wg.Wait()
			os.Exit(0)

		case msg := <-appLoggingCh:
			fmt.Println(msg.(string))

		case msg := <-radioLoggingCh:
			if err := status.sendLogMsg(msg.(string)); err != nil {
				fmt.Println(err)
			}

		case ev := <-connectionStatusCh:
			connStatus := ev.(int)
			fmt.Println("connstatus:", connStatus)
			if connStatus == comms.CONNECTED {
				status.online = true
				if err := status.sendUpdate(); err != nil {
					fmt.Println(err)
				}
			} else {
				status.online = false
			}
		}
	}
}

type serverStatus struct {
	online      bool
	statusTopic string
	logTopic    string
	toWireCh    chan comms.IOMsg
}

func (s *serverStatus) sendUpdate() error {

	msg := sbStatus.Status{}
	msg.Online = s.online
	data, err := msg.Marshal()
	if err != nil {
		return err
	}

	m := comms.IOMsg{}
	m.Data = data
	m.Topic = s.statusTopic
	m.Retain = true

	s.toWireCh <- m

	return nil
}

func createLastWillMsg() ([]byte, error) {

	willMsg := sbStatus.Status{}
	willMsg.Online = false
	data, err := willMsg.Marshal()

	return data, err
}

func (s *serverStatus) sendLogMsg(msg string) error {

	if !s.online {
		return errors.New("Can not publish log message since server is offline")
	}

	e := sbLog.LogMsg{}
	e.Level = sbLog.LOG_LEVEL_WARNING
	e.Msg = msg

	ba, err := e.Marshal()
	if err != nil {
		return err
	}

	m := comms.IOMsg{}
	m.Data = ba
	m.Topic = s.logTopic

	s.toWireCh <- m

	fmt.Println(msg)

	return nil
}
