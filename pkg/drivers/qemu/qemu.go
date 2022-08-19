/*
Copyright 2018 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package qemu

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/docker/machine/libmachine/drivers"
	"github.com/docker/machine/libmachine/log"
	"github.com/docker/machine/libmachine/mcnutils"
	"github.com/docker/machine/libmachine/ssh"
	"github.com/docker/machine/libmachine/state"
	"github.com/pkg/errors"

	pkgdrivers "k8s.io/minikube/pkg/drivers"
)

const (
	isoFilename        = "boot2docker.iso"
	privateNetworkName = "docker-machines"

	defaultSSHUser = "docker"
)

type Driver struct {
	*drivers.BaseDriver
	*pkgdrivers.CommonDriver
	EnginePort int
	FirstQuery bool

	Memory           int
	DiskSize         int
	CPU              int
	Program          string
	BIOS             bool
	CPUType          string
	MachineType      string
	Firmware         string
	Display          bool
	DisplayType      string
	Nographic        bool
	VirtioDrives     bool
	Network          string
	PrivateNetwork   string
	Boot2DockerURL   string
	NetworkInterface string
	NetworkAddress   string
	NetworkSocket    string
	NetworkBridge    string
	CaCertPath       string
	PrivateKeyPath   string
	DiskPath         string
	CacheMode        string
	IOMode           string
	UserDataFile     string
	CloudConfigRoot  string
	LocalPorts       string
}

func (d *Driver) GetMachineName() string {
	return d.MachineName
}

func (d *Driver) GetSSHHostname() (string, error) {
	return "localhost", nil
}

func (d *Driver) GetSSHKeyPath() string {
	return d.ResolveStorePath("id_rsa")
}

func (d *Driver) GetSSHPort() (int, error) {
	if d.SSHPort == 0 {
		d.SSHPort = 22
	}
	return d.SSHPort, nil
}

func (d *Driver) GetSSHUsername() string {
	if d.SSHUser == "" {
		d.SSHUser = "docker"
	}
	return d.SSHUser
}

func (d *Driver) DriverName() string {
	return "qemu2"
}

func (d *Driver) GetURL() (string, error) {
	if _, err := os.Stat(d.pidfilePath()); err != nil {
		return "", nil
	}
	ip, err := d.GetIP()
	if err != nil {
		log.Warnf("Failed to get IP: %v", err)
		return "", err
	}
	if ip == "" {
		return "", nil
	}
	port := d.GetPort()
	return fmt.Sprintf("tcp://%s:%d", ip, port), nil
}

func NewDriver(hostName, storePath string) drivers.Driver {
	return &Driver{
		BIOS:           runtime.GOARCH != "arm64",
		PrivateNetwork: privateNetworkName,
		BaseDriver: &drivers.BaseDriver{
			SSHUser:     defaultSSHUser,
			MachineName: hostName,
			StorePath:   storePath,
		},
	}
}

func (d *Driver) GetIP() (string, error) {
	if d.Network == "user" {
		return "127.0.0.1", nil
	}
	return d.NetworkAddress, nil
}

func (d *Driver) GetPort() int {
	var port = d.EnginePort
	if d.FirstQuery {
		d.FirstQuery = false
		port = 2376
	}
	return port
}

func checkPid(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Signal(syscall.Signal(0))
}

func (d *Driver) GetState() (state.State, error) {
	if _, err := os.Stat(d.pidfilePath()); err != nil {
		return state.Stopped, nil
	}
	p, err := os.ReadFile(d.pidfilePath())
	if err != nil {
		return state.Error, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(p)))
	if err != nil {
		return state.Error, err
	}
	if err := checkPid(pid); err != nil {
		// No pid, remove pidfile
		os.Remove(d.pidfilePath())
		return state.Stopped, nil
	}
	ret, err := d.RunQMPCommand("query-status")
	if err != nil {
		return state.Error, err
	}

	// RunState is one of:
	// 'debug', 'inmigrate', 'internal-error', 'io-error', 'paused',
	// 'postmigrate', 'prelaunch', 'finish-migrate', 'restore-vm',
	// 'running', 'save-vm', 'shutdown', 'suspended', 'watchdog',
	// 'guest-panicked'
	switch ret["status"] {
	case "running":
		return state.Running, nil
	case "paused":
		return state.Paused, nil
	case "shutdown":
		return state.Stopped, nil
	}
	return state.None, nil
}

func (d *Driver) PreCreateCheck() error {
	return nil
}

func (d *Driver) Create() error {
	var err error
	if d.Network == "user" {
		minPort, maxPort, err := parsePortRange(d.LocalPorts)
		log.Debugf("port range: %d -> %d", minPort, maxPort)
		if err != nil {
			return err
		}
		d.SSHPort, err = getAvailableTCPPortFromRange(minPort, maxPort)
		if err != nil {
			return err
		}

		for {
			d.EnginePort, err = getAvailableTCPPortFromRange(minPort, maxPort)
			if err != nil {
				return err
			}
			if d.EnginePort == d.SSHPort {
				// can't have both on same port
				continue
			}
			break
		}
	}
	b2dutils := mcnutils.NewB2dUtils(d.StorePath)
	if err := b2dutils.CopyIsoToMachineDir(d.Boot2DockerURL, d.MachineName); err != nil {
		return err
	}

	log.Info("Creating SSH key...")
	if err := ssh.GenerateSSHKey(d.sshKeyPath()); err != nil {
		return err
	}

	log.Info("Creating Disk image...")
	if err := d.generateDiskImage(d.DiskSize); err != nil {
		return err
	}

	if d.UserDataFile != "" {
		log.Info("Creating Userdata Disk...")
		if d.CloudConfigRoot, err = d.generateUserdataDisk(d.UserDataFile); err != nil {
			return err
		}
	}

	log.Info("Starting QEMU VM...")
	return d.Start()
}

func parsePortRange(rawPortRange string) (int, int, error) {
	if rawPortRange == "" {
		return 0, 65535, nil
	}

	portRange := strings.Split(rawPortRange, "-")

	if len(portRange) < 2 {
		return 0, 0, errors.New("invalid port range, requires at least 2 ports")
	}

	minPort, err := strconv.Atoi(portRange[0])
	if err != nil {
		return 0, 0, errors.Wrap(err, "invalid min port range")
	}

	maxPort, err := strconv.Atoi(portRange[1])
	if err != nil {
		return 0, 0, errors.Wrap(err, "invalid max port range")
	}

	if maxPort < minPort {
		return 0, 0, errors.New("invalid port range, max less than min")
	}

	if maxPort-minPort < 2 {
		return 0, 0, errors.New("invalid port range, requires at least 2 ports")
	}
	return minPort, maxPort, nil
}

func getRandomPortNumberInRange(min, max int) int {
	return rand.Intn(max-min) + min
}

func getAvailableTCPPortFromRange(minPort, maxPort int) (int, error) {
	port := 0
	for i := 0; i <= 10; i++ {
		var ln net.Listener
		var err error
		if minPort == 0 && maxPort == 65535 {
			ln, err = net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				return 0, err
			}
		} else {
			port = getRandomPortNumberInRange(minPort, maxPort)
			log.Debugf("testing port: %d", port)
			ln, err = net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
			if err != nil {
				log.Debugf("port already in use: %d", port)
				continue
			}
		}
		defer ln.Close()
		addr := ln.Addr().String()
		addrParts := strings.SplitN(addr, ":", 2)
		p, err := strconv.Atoi(addrParts[1])
		if err != nil {
			return 0, err
		}
		if p != 0 {
			port = p
			return port, nil
		}
		time.Sleep(time.Second)
	}
	return 0, fmt.Errorf("unable to allocate tcp port")
}

func (d *Driver) Start() error {
	machineDir := filepath.Join(d.StorePath, "machines", d.GetMachineName())

	var startCmd []string

	if d.MachineType != "" {
		machineType := d.MachineType
		startCmd = append(startCmd,
			"-M", machineType,
		)
	}

	if d.CPUType != "" {
		startCmd = append(startCmd,
			"-cpu", d.CPUType,
		)
	}

	if !d.BIOS {
		if d.Firmware != "" {
			startCmd = append(startCmd,
				"-drive", fmt.Sprintf("file=%s,readonly=on,format=raw,if=pflash", d.Firmware))
		} else {
			return fmt.Errorf("unknown firmware")
		}
	}

	if d.Display {
		if d.DisplayType != "" {
			startCmd = append(startCmd,
				"-display", d.DisplayType,
			)
		}
		// else use the default graphic output
	} else {
		if d.Nographic {
			startCmd = append(startCmd,
				"-nographic",
			)
		} else {
			startCmd = append(startCmd,
				"-display", "none",
			)
		}
	}

	// hardware acceleration is important, it increases performance by 10x
	if runtime.GOOS == "darwin" {
		// On macOS, enable the Hypervisor framework accelerator.
		startCmd = append(startCmd,
			"-accel", "hvf")
	} else if _, err := os.Stat("/dev/kvm"); err == nil && runtime.GOOS == "linux" {
		// On Linux, enable the Kernel Virtual Machine accelerator.
		startCmd = append(startCmd,
			"-accel", "kvm")
	}

	startCmd = append(startCmd,
		"-m", fmt.Sprintf("%d", d.Memory),
		"-smp", fmt.Sprintf("%d", d.CPU),
		"-boot", "d")
	var isoPath = filepath.Join(machineDir, isoFilename)
	if d.VirtioDrives {
		startCmd = append(startCmd,
			"-drive", fmt.Sprintf("file=%s,index=2,media=cdrom,if=virtio", isoPath))
	} else {
		startCmd = append(startCmd,
			"-cdrom", isoPath)
	}

	// if runtime.GOOS != "windows" {
	startCmd = append(startCmd,

		"-qmp", fmt.Sprintf("tcp:%s,server,nowait", d.monitorPath()),
		// "-qmp", fmt.Sprintf("unix:%s,server,nowait", d.monitorPath()),
		"-pidfile", d.pidfilePath(),
	)
	// }

	switch d.Network {
	case "user":
		startCmd = append(startCmd,
			"-nic", fmt.Sprintf("user,model=virtio,hostfwd=tcp::%d-:22,hostfwd=tcp::%d-:2376,hostname=%s", d.SSHPort, d.EnginePort, d.GetMachineName()),
		)
	case "tap":
		startCmd = append(startCmd,
			"-nic", fmt.Sprintf("tap,model=virtio,ifname=%s,script=no,downscript=no", d.NetworkInterface),
		)
	case "vde":
		startCmd = append(startCmd,
			"-nic", fmt.Sprintf("vde,model=virtio,sock=%s", d.NetworkSocket),
		)
	case "bridge":
		startCmd = append(startCmd,
			"-nic", fmt.Sprintf("bridge,model=virtio,br=%s", d.NetworkBridge),
		)
	default:
		log.Errorf("unknown network: %s", d.Network)
	}

	if runtime.GOOS != "windows" {
		//Daemonize not available in Windows. Will need to use 'start' command
		startCmd = append(startCmd,
			"-daemonize")
	}

	if d.CloudConfigRoot != "" {
		startCmd = append(startCmd,
			"-fsdev",
			fmt.Sprintf("local,security_model=passthrough,readonly,id=fsdev0,path=%s", d.CloudConfigRoot))
		startCmd = append(startCmd,
			"-device",
			"virtio-9p-pci,id=fs0,fsdev=fsdev0,mount_tag=config-2")
	}

	if d.VirtioDrives {
		startCmd = append(startCmd,
			"-drive", fmt.Sprintf("file=%s,index=0,media=disk,if=virtio", d.diskPath()))
	} else {
		// last argument is always the name of the disk image
		startCmd = append(startCmd,
			d.diskPath())
	}

	if runtime.GOOS == "windows" {
		//Use the start command
		var s = append([]string{"/c start"}, d.Program)
		startCmd = append(s, startCmd...)
		if stdout, stderr, err := cmdOutErr("cmd", startCmd...); err != nil {
			fmt.Printf("OUTPUT: %s\n", stdout)
			fmt.Printf("ERROR: %s\n", stderr)
			return err
		}
	} else {
		if stdout, stderr, err := cmdOutErr(d.Program, startCmd...); err != nil {
			fmt.Printf("OUTPUT: %s\n", stdout)
			fmt.Printf("ERROR: %s\n", stderr)
			return err
		}
	}
	log.Infof("Waiting for VM to start (ssh -p %d docker@localhost)...", d.SSHPort)

	return WaitForTCPWithDelay(fmt.Sprintf("localhost:%d", d.SSHPort), time.Second)
}

func cmdOutErr(cmdStr string, args ...string) (string, string, error) {
	cmd := exec.Command(cmdStr, args...)
	log.Debugf("executing: %v %v", cmdStr, strings.Join(args, " "))
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	stdoutStr := stdout.String()
	stderrStr := stderr.String()
	log.Debugf("STDOUT: %v", stdoutStr)
	log.Debugf("STDERR: %v", stderrStr)
	if err != nil {
		if ee, ok := err.(*exec.Error); ok && ee == exec.ErrNotFound {
			err = fmt.Errorf("mystery error: %v", ee)
		}
	} else {
		// also catch error messages in stderr, even if the return code looks OK
		if strings.Contains(stderrStr, "error:") {
			err = fmt.Errorf("%s %s failed: %s", cmdStr, strings.Join(args, " "), stderrStr)
		}
	}
	return stdoutStr, stderrStr, err
}

func (d *Driver) Stop() error {
	if _, err := d.RunQMPCommand("system_powerdown"); err != nil {
		return err
	}
	return nil
}

func (d *Driver) Remove() error {
	s, err := d.GetState()
	if err != nil {
		return errors.Wrap(err, "get state")
	}
	if s == state.Running {
		if err := d.Kill(); err != nil {
			return errors.Wrap(err, "kill")
		}
	}
	if s != state.Stopped {
		if _, err := d.RunQMPCommand("quit"); err != nil {
			return errors.Wrap(err, "quit")
		}
	}
	return nil
}

func (d *Driver) Restart() error {
	s, err := d.GetState()
	if err != nil {
		return err
	}

	if s == state.Running {
		if err := d.Stop(); err != nil {
			return err
		}
	}
	return d.Start()
}

func (d *Driver) Kill() error {
	if _, err := d.RunQMPCommand("system_powerdown"); err != nil {
		return err
	}
	return nil
}

func (d *Driver) StartDocker() error {
	return fmt.Errorf("hosts without a driver cannot start docker")
}

func (d *Driver) StopDocker() error {
	return fmt.Errorf("hosts without a driver cannot stop docker")
}

func (d *Driver) GetDockerConfigDir() string {
	return ""
}

func (d *Driver) Upgrade() error {
	return fmt.Errorf("hosts without a driver cannot be upgraded")
}

func (d *Driver) sshKeyPath() string {
	machineDir := filepath.Join(d.StorePath, "machines", d.GetMachineName())
	return filepath.Join(machineDir, "id_rsa")
}

func (d *Driver) publicSSHKeyPath() string {
	return d.sshKeyPath() + ".pub"
}

func (d *Driver) diskPath() string {
	machineDir := filepath.Join(d.StorePath, "machines", d.GetMachineName())
	return filepath.Join(machineDir, "disk.qcow2")
}

func (d *Driver) monitorPath() string {
	// machineDir := filepath.Join(d.StorePath, "machines", d.GetMachineName())
	// return filepath.Join(machineDir, "monitor")
	return "localhost:4444"
}

func (d *Driver) pidfilePath() string {
	machineDir := filepath.Join(d.StorePath, "machines", d.GetMachineName())
	return filepath.Join(machineDir, "qemu.pid")
}

// Make a boot2docker VM disk image.
func (d *Driver) generateDiskImage(size int) error {
	log.Debugf("Creating %d MB hard disk image...", size)

	magicString := "boot2docker, please format-me"

	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)

	// magicString first so the automount script knows to format the disk
	file := &tar.Header{Name: magicString, Size: int64(len(magicString))}
	if err := tw.WriteHeader(file); err != nil {
		return err
	}
	if _, err := tw.Write([]byte(magicString)); err != nil {
		return err
	}
	// .ssh/key.pub => authorized_keys
	file = &tar.Header{Name: ".ssh", Typeflag: tar.TypeDir, Mode: 0700}
	if err := tw.WriteHeader(file); err != nil {
		return err
	}
	pubKey, err := os.ReadFile(d.publicSSHKeyPath())
	if err != nil {
		return err
	}
	file = &tar.Header{Name: ".ssh/authorized_keys", Size: int64(len(pubKey)), Mode: 0644}
	if err := tw.WriteHeader(file); err != nil {
		return err
	}
	if _, err := tw.Write(pubKey); err != nil {
		return err
	}
	file = &tar.Header{Name: ".ssh/authorized_keys2", Size: int64(len(pubKey)), Mode: 0644}
	if err := tw.WriteHeader(file); err != nil {
		return err
	}
	if _, err := tw.Write(pubKey); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	rawFile := fmt.Sprintf("%s.raw", d.diskPath())
	if err := os.WriteFile(rawFile, buf.Bytes(), 0644); err != nil {
		return nil
	}
	if stdout, stderr, err := cmdOutErr("qemu-img", "convert", "-f", "raw", "-O", "qcow2", rawFile, d.diskPath()); err != nil {
		fmt.Printf("OUTPUT: %s\n", stdout)
		fmt.Printf("ERROR: %s\n", stderr)
		return err
	}
	if stdout, stderr, err := cmdOutErr("qemu-img", "resize", d.diskPath(), fmt.Sprintf("+%dM", size)); err != nil {
		fmt.Printf("OUTPUT: %s\n", stdout)
		fmt.Printf("ERROR: %s\n", stderr)
		return err
	}
	log.Debugf("DONE writing to %s and %s", rawFile, d.diskPath())
	return nil
}

func (d *Driver) generateUserdataDisk(userdataFile string) (string, error) {
	// Start with virtio, add ISO & FAT format later
	// Start with local file, add wget/fetct URL? (or if URL, use datasource..)
	userdata, err := os.ReadFile(userdataFile)
	if err != nil {
		return "", err
	}

	machineDir := filepath.Join(d.StorePath, "machines", d.GetMachineName())
	ccRoot := filepath.Join(machineDir, "cloud-config")
	err = os.MkdirAll(ccRoot, 0755)
	if err != nil {
		return "", err
	}

	userDataDir := filepath.Join(ccRoot, "openstack/latest")
	err = os.MkdirAll(userDataDir, 0755)
	if err != nil {
		return "", err
	}

	writeFile := filepath.Join(userDataDir, "user_data")
	if err := os.WriteFile(writeFile, userdata, 0644); err != nil {
		return "", err
	}
	return ccRoot, nil
}

func (d *Driver) RunQMPCommand(command string) (map[string]interface{}, error) {
	// connect to monitor
	conn, err := net.Dial("tcp", d.monitorPath())
	// conn, err := net.Dial("unix", d.monitorPath())
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// initial QMP response
	var buf [1024]byte
	nr, err := conn.Read(buf[:])
	if err != nil {
		return nil, err
	}
	type qmpInitialResponse struct {
		QMP struct {
			Version struct {
				QEMU struct {
					Micro int `json:"micro"`
					Minor int `json:"minor"`
					Major int `json:"major"`
				} `json:"qemu"`
				Package string `json:"package"`
			} `json:"version"`
			Capabilities []string `json:"capabilities"`
		} `jason:"QMP"`
	}

	var initialResponse qmpInitialResponse
	err = json.Unmarshal(buf[:nr], &initialResponse)
	if err != nil {
		return nil, err
	}

	// run 'qmp_capabilities' to switch to command mode
	// { "execute": "qmp_capabilities" }
	type qmpCommand struct {
		Command string `json:"execute"`
	}
	jsonCommand, err := json.Marshal(qmpCommand{Command: "qmp_capabilities"})
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(jsonCommand); err != nil {
		return nil, err
	}
	nr, err = conn.Read(buf[:])
	if err != nil {
		return nil, err
	}
	type qmpResponse struct {
		Return map[string]interface{} `json:"return"`
	}
	var response qmpResponse
	err = json.Unmarshal(buf[:nr], &response)
	if err != nil {
		return nil, err
	}
	// expecting empty response
	if len(response.Return) != 0 {
		return nil, fmt.Errorf("qmp_capabilities failed: %v", response.Return)
	}

	// { "execute": command }
	jsonCommand, err = json.Marshal(qmpCommand{Command: command})
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(jsonCommand); err != nil {
		return nil, err
	}
	nr, err = conn.Read(buf[:])
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(buf[:nr], &response)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(command, "query-") {
		return response.Return, nil
	}
	// non-query commands should return an empty response
	if len(response.Return) != 0 {
		return nil, fmt.Errorf("%s failed: %v", command, response.Return)
	}
	return response.Return, nil
}

func WaitForTCPWithDelay(addr string, duration time.Duration) error {
	for {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			continue
		}
		defer conn.Close()
		if _, err := conn.Read(make([]byte, 1)); err != nil {
			time.Sleep(duration)
			continue
		}
		break
	}
	return nil
}
