package servermanager

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JustaPenguin/assetto-server-manager/pkg/udp"

	"github.com/sirupsen/logrus"
)

const MaxLogSizeBytes = 1e6

type ServerProcess interface {
	Start(event RaceEvent, udpPluginAddress string, udpPluginLocalPort int, forwardingAddress string, forwardListenPort int) error
	Stop() error
	Restart() error
	IsRunning() bool
	Event() RaceEvent
	UDPCallback(message udp.Message)
	SendUDPMessage(message udp.Message) error
	NotifyDone(chan struct{})
	Logs() string
}

// AssettoServerProcess manages the Assetto Corsa Server process.
type AssettoServerProcess struct {
	store                 Store
	contentManagerWrapper *ContentManagerWrapper

	start                 chan RaceEvent
	started, stopped, run chan error
	stop                  chan struct{}
	notifyDoneChs         []chan struct{}

	ctx context.Context
	cfn context.CancelFunc

	logBuffer *logBuffer

	raceEvent      RaceEvent
	cmd            *exec.Cmd
	mutex          sync.Mutex
	extraProcesses []*exec.Cmd

	// udp
	callbackFunc       udp.CallbackFunc
	udpServerConn      *udp.AssettoServerUDP
	udpPluginAddress   string
	udpPluginLocalPort int
	forwardingAddress  string
	forwardListenPort  int
}

func NewAssettoServerProcess(callbackFunc udp.CallbackFunc, store Store, contentManagerWrapper *ContentManagerWrapper) *AssettoServerProcess {
	sp := &AssettoServerProcess{
		start:                 make(chan RaceEvent),
		stop:                  make(chan struct{}),
		started:               make(chan error),
		stopped:               make(chan error),
		run:                   make(chan error),
		logBuffer:             newLogBuffer(MaxLogSizeBytes),
		callbackFunc:          callbackFunc,
		store:                 store,
		contentManagerWrapper: contentManagerWrapper,
	}

	go sp.loop()

	return sp
}

func (sp *AssettoServerProcess) UDPCallback(message udp.Message) {
	panicCapture(func() {
		sp.callbackFunc(message)
	})
}

func (sp *AssettoServerProcess) Start(event RaceEvent, udpPluginAddress string, udpPluginLocalPort int, forwardingAddress string, forwardListenPort int) error {
	sp.mutex.Lock()
	sp.udpPluginAddress = udpPluginAddress
	sp.udpPluginLocalPort = udpPluginLocalPort
	sp.forwardingAddress = forwardingAddress
	sp.forwardListenPort = forwardListenPort
	sp.mutex.Unlock()

	sp.start <- event

	return <-sp.started
}

func (sp *AssettoServerProcess) IsRunning() bool {
	sp.mutex.Lock()
	defer sp.mutex.Unlock()

	return sp.raceEvent != nil
}

var ErrServerProcessTimeout = errors.New("servermanager: server process did not stop even after manual kill. please check your server configuration")

func (sp *AssettoServerProcess) Stop() error {
	timeout := time.After(time.Second * 10)
	fullTimeout := time.After(time.Second * 20)
	sp.cfn()

	for {
		select {
		case err := <-sp.stopped:
			return err
		case <-timeout:
			// @TODO there needs to be some exit condition here...
			sp.mutex.Lock()
			logrus.Debug("Server process did not naturally stop after 10s. Attempting manual kill.")
			err := kill(sp.cmd.Process)

			if err != nil {
				logrus.WithError(err).Error("Could not forcibly kill command")
			}
			sp.mutex.Unlock()
		case <-fullTimeout:
			return ErrServerProcessTimeout
		}
	}
}

func (sp *AssettoServerProcess) Restart() error {
	running := sp.IsRunning()

	sp.mutex.Lock()
	raceEvent := sp.raceEvent
	udpPluginAddress := sp.udpPluginAddress
	udpLocalPluginPort := sp.udpPluginLocalPort
	forwardingAddress := sp.forwardingAddress
	forwardListenPort := sp.forwardListenPort
	sp.mutex.Unlock()

	if running {
		if err := sp.Stop(); err != nil {
			return err
		}
	}

	return sp.Start(raceEvent, udpPluginAddress, udpLocalPluginPort, forwardingAddress, forwardListenPort)
}

func (sp *AssettoServerProcess) loop() {
	for {
		select {
		case err := <-sp.run:
			if err != nil {
				logrus.WithError(err).Warn("acServer process ended with error. If everything seems fine, you can safely ignore this error.")
			}

			select {
			case sp.stopped <- sp.onStop():
			default:
			}
		case raceEvent := <-sp.start:
			if sp.IsRunning() {
				if err := sp.Stop(); err != nil {
					sp.started <- err
					break
				}
			}

			sp.started <- sp.startRaceEvent(raceEvent)
		}
	}
}

func (sp *AssettoServerProcess) startRaceEvent(raceEvent RaceEvent) error {
	sp.mutex.Lock()
	defer sp.mutex.Unlock()

	logrus.Infof("Starting Server Process with event: %s", describeRaceEvent(raceEvent))
	var executablePath string

	if filepath.IsAbs(config.Steam.ExecutablePath) {
		executablePath = config.Steam.ExecutablePath
	} else {
		executablePath = filepath.Join(ServerInstallPath, config.Steam.ExecutablePath)
	}

	sp.ctx, sp.cfn = context.WithCancel(context.Background())
	sp.cmd = buildCommand(sp.ctx, executablePath)
	sp.cmd.Dir = ServerInstallPath

	sp.cmd.Stdout = sp.logBuffer
	sp.cmd.Stderr = sp.logBuffer

	if err := sp.startUDPListener(); err != nil {
		return err
	}

	wd, err := os.Getwd()

	if err != nil {
		return err
	}

	sp.raceEvent = raceEvent

	go func() {
		sp.run <- sp.cmd.Run()
	}()

	serverOptions, err := sp.store.LoadServerOptions()

	if err != nil {
		return err
	}

	if serverOptions.EnableContentManagerWrapper == 1 && serverOptions.ContentManagerWrapperPort > 0 {
		go func() {
			err := sp.contentManagerWrapper.Start(serverOptions.ContentManagerWrapperPort, sp.raceEvent)

			if err != nil {
				logrus.WithError(err).Error("Could not start Content Manager wrapper server")
			}
		}()
	}

	if strackerOptions, err := sp.store.LoadStrackerOptions(); err == nil && strackerOptions.EnableStracker && IsStrackerInstalled() {
		if serverOptions.UDPPluginLocalPort >= 0 && serverOptions.UDPPluginAddress != "" || strings.Contains(serverOptions.UDPPluginAddress, ":") {
			strackerOptions.ACPlugin.SendPort = serverOptions.UDPPluginLocalPort
			strackerOptions.ACPlugin.ReceivePort = formValueAsInt(strings.Split(serverOptions.UDPPluginAddress, ":")[1])

			err = strackerOptions.Write()

			if err != nil {
				return err
			}

			err = sp.startPlugin(wd, &CommandPlugin{
				Executable: StrackerExecutablePath(),
				Arguments: []string{
					"--stracker_ini",
					filepath.Join(StrackerFolderPath(), strackerConfigIniFilename),
				},
			})

			if err != nil {
				return err
			}

			logrus.Infof("Started sTracker. Listening for pTracker connections on port %d", strackerOptions.InstanceConfiguration.ListeningPort)
		} else {
			logrus.WithError(ErrStrackerConfigurationRequiresUDPPluginConfiguration).Error("Please check your server configuration")
		}
	}

	for _, plugin := range config.Server.Plugins {
		err = sp.startPlugin(wd, plugin)

		if err != nil {
			logrus.WithError(err).Errorf("Could not run extra command: %s", plugin.String())
		}
	}

	if len(config.Server.RunOnStart) > 0 {
		logrus.Warnf("Use of run_on_start in config.yml is deprecated. Please use 'plugins' instead")

		for _, command := range config.Server.RunOnStart {
			err = sp.startChildProcess(wd, command)

			if err != nil {
				logrus.WithError(err).Errorf("Could not run extra command: %s", command)
			}
		}
	}

	return nil
}

func (sp *AssettoServerProcess) onStop() error {
	sp.mutex.Lock()
	defer sp.mutex.Unlock()
	logrus.Debugf("Server stopped. Stopping UDP listener and child processes.")

	sp.raceEvent = nil

	if err := sp.stopUDPListener(); err != nil {
		return err
	}

	sp.stopChildProcesses()

	for _, doneCh := range sp.notifyDoneChs {
		select {
		case doneCh <- struct{}{}:
		default:
		}
	}

	return nil
}

func (sp *AssettoServerProcess) Logs() string {
	return sp.logBuffer.String()
}

func (sp *AssettoServerProcess) Event() RaceEvent {
	sp.mutex.Lock()
	defer sp.mutex.Unlock()

	return sp.raceEvent
}

var ErrNoOpenUDPConnection = errors.New("servermanager: no open UDP connection found")

func (sp *AssettoServerProcess) SendUDPMessage(message udp.Message) error {
	sp.mutex.Lock()
	defer sp.mutex.Unlock()

	if sp.udpServerConn == nil {
		return ErrNoOpenUDPConnection
	}

	return sp.udpServerConn.SendMessage(message)
}

func (sp *AssettoServerProcess) NotifyDone(ch chan struct{}) {
	sp.mutex.Lock()
	defer sp.mutex.Unlock()

	sp.notifyDoneChs = append(sp.notifyDoneChs, ch)
}

func (sp *AssettoServerProcess) startPlugin(wd string, plugin *CommandPlugin) error {
	commandFullPath, err := filepath.Abs(plugin.Executable)

	if err != nil {
		return err
	}

	ctx := context.Background()

	cmd := buildCommand(ctx, commandFullPath, plugin.Arguments...)

	pluginDir, err := filepath.Abs(filepath.Dir(commandFullPath))

	if err != nil {
		logrus.WithError(err).Warnf("Could not determine plugin directory. Setting working dir to: %s", wd)
		pluginDir = wd
	}

	cmd.Stdout = pluginsOutput
	cmd.Stderr = pluginsOutput

	cmd.Dir = pluginDir

	err = cmd.Start()

	if err != nil {
		return err
	}

	sp.extraProcesses = append(sp.extraProcesses, cmd)

	return nil
}

// Deprecated: use startPlugin instead
func (sp *AssettoServerProcess) startChildProcess(wd string, command string) error {
	// BUG(cj): splitting commands on spaces breaks child processes that have a space in their path name
	parts := strings.Split(command, " ")

	if len(parts) == 0 {
		return nil
	}

	commandFullPath, err := filepath.Abs(parts[0])

	if err != nil {
		return err
	}

	var cmd *exec.Cmd
	ctx := context.Background()

	if len(parts) > 1 {
		cmd = buildCommand(ctx, commandFullPath, parts[1:]...)
	} else {
		cmd = buildCommand(ctx, commandFullPath)
	}

	pluginDir, err := filepath.Abs(filepath.Dir(commandFullPath))

	if err != nil {
		logrus.WithError(err).Warnf("Could not determine plugin directory. Setting working dir to: %s", wd)
		pluginDir = wd
	}

	cmd.Stdout = pluginsOutput
	cmd.Stderr = pluginsOutput

	cmd.Dir = pluginDir

	err = cmd.Start()

	if err != nil {
		return err
	}

	sp.extraProcesses = append(sp.extraProcesses, cmd)

	return nil
}

func (sp *AssettoServerProcess) stopChildProcesses() {
	sp.contentManagerWrapper.Stop()

	for _, command := range sp.extraProcesses {
		err := kill(command.Process)

		if err != nil {
			logrus.WithError(err).Errorf("Can't kill process: %d", command.Process.Pid)
			continue
		}

		_ = command.Process.Release()
	}

	sp.extraProcesses = make([]*exec.Cmd, 0)
}

func (sp *AssettoServerProcess) startUDPListener() error {
	var err error

	host, portStr, err := net.SplitHostPort(sp.udpPluginAddress)

	if err != nil {
		return err
	}

	port, err := strconv.ParseInt(portStr, 10, 0)

	if err != nil {
		return err
	}

	sp.udpServerConn, err = udp.NewServerClient(host, int(port), sp.udpPluginLocalPort, true, sp.forwardingAddress, sp.forwardListenPort, sp.UDPCallback)

	if err != nil {
		return err
	}

	return nil
}

func (sp *AssettoServerProcess) stopUDPListener() error {
	return sp.udpServerConn.Close()
}

func newLogBuffer(maxSize int) *logBuffer {
	return &logBuffer{
		size: maxSize,
		buf:  new(bytes.Buffer),
	}
}

type logBuffer struct {
	buf *bytes.Buffer

	size int

	mutex sync.Mutex
}

func (lb *logBuffer) Write(p []byte) (n int, err error) {
	lb.mutex.Lock()
	defer lb.mutex.Unlock()

	b := lb.buf.Bytes()

	if len(b) > lb.size {
		lb.buf = bytes.NewBuffer(b[len(b)-lb.size:])
	}

	return lb.buf.Write(p)
}

func (lb *logBuffer) String() string {
	lb.mutex.Lock()
	defer lb.mutex.Unlock()

	return lb.buf.String()
}

func FreeUDPPort() (int, error) {
	addr, err := net.ResolveUDPAddr("udp", "localhost:0")

	if err != nil {
		return 0, err
	}

	l, err := net.ListenUDP("udp", addr)

	if err != nil {
		return 0, err
	}

	defer l.Close()

	return l.LocalAddr().(*net.UDPAddr).Port, nil
}
