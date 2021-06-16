package nvidia

import (
	"fmt"
	"log"
	"net"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/net/context"
	"google.golang.org/grpc"
	pluginapi "k8s.io/kubernetes/pkg/kubelet/apis/deviceplugin/v1beta1"
)

const (
	resourceName           = "nvidia.com/gpu"
	serverSock             = pluginapi.DevicePluginPath + "hkube-vgpu.sock"
	envDisableHealthChecks = "DP_DISABLE_HEALTHCHECKS"
	allHealthChecks        = "xids"
)

// NvidiaDevicePlugin implements the Kubernetes device plugin API
type NvidiaDevicePlugin struct {
	devs         []*pluginapi.Device
	physicalDevs []string

	socket string

	stop   chan interface{}
	health chan *pluginapi.Device

	server *grpc.Server
}

// NewNvidiaDevicePlugin returns an initialized NvidiaDevicePlugin
func NewNvidiaDevicePlugin(vGPUCount int) *NvidiaDevicePlugin {
	physicalDevs := getPhysicalGPUDevices()
	vGPUDevs := getVGPUDevices(vGPUCount)

	return &NvidiaDevicePlugin{
		devs:         vGPUDevs,
		physicalDevs: physicalDevs,
		socket:       serverSock,

		stop:   make(chan interface{}),
		health: make(chan *pluginapi.Device),
	}
}

func (m *NvidiaDevicePlugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{}, nil
}

// dial establishes the gRPC communication with the registered device plugin.
func dial(unixSocketPath string, timeout time.Duration) (*grpc.ClientConn, error) {
	c, err := grpc.Dial(unixSocketPath, grpc.WithInsecure(), grpc.WithBlock(),
		grpc.WithTimeout(timeout),
		grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("unix", addr, timeout)
		}),
	)

	if err != nil {
		return nil, err
	}

	return c, nil
}

// Start starts the gRPC server of the device plugin
func (m *NvidiaDevicePlugin) Start() error {
	err := m.cleanup()
	if err != nil {
		return err
	}

	sock, err := net.Listen("unix", m.socket)
	if err != nil {
		return err
	}

	m.server = grpc.NewServer([]grpc.ServerOption{}...)
	pluginapi.RegisterDevicePluginServer(m.server, m)

	go func() {
		lastCrashTime := time.Now()
		restartCount := 0
		for {
			log.Println("Starting GRPC server")
			err := m.server.Serve(sock)
			if err != nil {
				log.Printf("GRPC server crashed with error: %v", err)
			}
			// restart if it has not been too often
			// i.e. if server has crashed more than 5 times and it didn't last more than one hour each time
			if restartCount > 5 {
				// quit
				log.Fatal("GRPC server has repeatedly crashed recently. Quitting")
			}
			timeSinceLastCrash := time.Since(lastCrashTime).Seconds()
			lastCrashTime = time.Now()
			if timeSinceLastCrash > 3600 {
				// it has been one hour since the last crash.. reset the count
				// to reflect on the frequency
				restartCount = 1
			} else {
				restartCount += 1
			}
		}
	}()

	// Wait for server to start by launching a blocking connexion
	conn, err := dial(m.socket, 5*time.Second)
	if err != nil {
		return err
	}
	conn.Close()

	// go m.healthcheck()

	return nil
}

// Stop stops the gRPC server
func (m *NvidiaDevicePlugin) Stop() error {
	if m.server == nil {
		return nil
	}

	m.server.Stop()
	m.server = nil
	close(m.stop)

	return m.cleanup()
}

// Register registers the device plugin for the given resourceName with Kubelet.
func (m *NvidiaDevicePlugin) Register(kubeletEndpoint, resourceName string) error {
	conn, err := dial(kubeletEndpoint, 5*time.Second)
	if err != nil {
		log.Printf("endpoint %s, Dial conn error: %s", kubeletEndpoint, err)
		return err
	}
	defer conn.Close()

	client := pluginapi.NewRegistrationClient(conn)
	reqt := &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     path.Base(m.socket),
		ResourceName: resourceName,
	}

	_, err = client.Register(context.Background(), reqt)
	if err != nil {
		log.Printf("client register: %s", err)
		return err
	}
	return nil
}

// ListAndWatch lists devices and update that list according to the health status
func (m *NvidiaDevicePlugin) ListAndWatch(e *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	s.Send(&pluginapi.ListAndWatchResponse{Devices: m.devs})

	for {
		select {
		case <-m.stop:
			return nil
		case d := <-m.health:
			// FIXME: there is no way to recover from the Unhealthy state.
			d.Health = pluginapi.Unhealthy
			log.Printf("device marked unhealthy: %s", d.ID)
			s.Send(&pluginapi.ListAndWatchResponse{Devices: m.devs})
		}
	}
}

func (m *NvidiaDevicePlugin) unhealthy(dev *pluginapi.Device) {
	m.health <- dev
}

// Allocate which return list of devices.
func (m *NvidiaDevicePlugin) Allocate(ctx context.Context, reqs *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	devs := m.devs
	responses := pluginapi.AllocateResponse{}
	physicalDevsMap := make(map[string]bool)
	for _, req := range reqs.ContainerRequests {
		for _, id := range req.DevicesIDs {
			if !deviceExists(devs, id) {
				return nil, fmt.Errorf("invalid allocation request: unknown device: %s", id)
			}

			// Convert virtual GPUDeviceId to physical GPUDeviceID
			physicalDevId := getPhysicalDeviceID(id)
			if !physicalDevsMap[physicalDevId] {
				physicalDevsMap[physicalDevId] = true
			}

			dev := getDeviceById(devs, id)
			if dev == nil {
				return nil, fmt.Errorf("invalid allocation request: unknown device: %s", id)
			}

			if dev.Health != pluginapi.Healthy {
				return nil, fmt.Errorf("invalid allocation request with unhealthy device %s", id)
			}
		}

		// Set physical GPU devices as container visible devices
		visibleDevs := make([]string, 0, len(physicalDevsMap))
		for visibleDev := range physicalDevsMap {
			visibleDevs = append(visibleDevs, visibleDev)
		}
		response := pluginapi.ContainerAllocateResponse{
			Envs: map[string]string{
				"NVIDIA_VISIBLE_DEVICES": strings.Join(visibleDevs, ","),
			},
		}

		// Set MPS environment variables - figure it out why it doesn't work?
		//response.Envs["CUDA_MPS_ACTIVE_THREAD_PERCENTAGE"] = fmt.Sprintf("%d", 100 * uint(len(req.DevicesIDs) / len(m.devs)))
		//response.Envs["CUDA_MPS_PIPE_DIRECTORY"] = "/tmp"
		//
		response.Mounts = append(response.Mounts, &pluginapi.Mount{
			HostPath: "/home/kubernetes/bin/nvidia",
			ContainerPath: "/usr/local/nvidia",
		})
		response.Mounts = append(response.Mounts, &pluginapi.Mount{
			ContainerPath: "/etc/vulkan/icd.d",
			HostPath: "/home/kubernetes/bin/vulkan/icd.d",
		})
		response.Devices = append(response.Devices, &pluginapi.DeviceSpec{
			HostPath:      "/dev/nvidia0",
			ContainerPath: "/dev/nvidia0",
			Permissions:   "mrw",
		})
		response.Devices = append(response.Devices, &pluginapi.DeviceSpec{
			HostPath:      "/dev/nvidiactl",
			ContainerPath: "/dev/nvidiactl",
			Permissions:   "mrw",
		})
		response.Devices = append(response.Devices, &pluginapi.DeviceSpec{
			HostPath:      "/dev/nvidia-uvm",
			ContainerPath: "/dev/nvidia-uvm",
			Permissions:   "mrw",
		})


		responses.ContainerResponses = append(responses.ContainerResponses, &response)
	}

	return &responses, nil
}

func (m *NvidiaDevicePlugin) PreStartContainer(context.Context, *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}

func (m *NvidiaDevicePlugin) cleanup() error {
	if err := os.Remove(m.socket); err != nil && !os.IsNotExist(err) {
		return err
	}

	return nil
}

// Need to make sure all health check check against real device but not the virtual device

func (m *NvidiaDevicePlugin) healthcheck() {
	disableHealthChecks := strings.ToLower(os.Getenv(envDisableHealthChecks))
	if disableHealthChecks == "all" {
		disableHealthChecks = allHealthChecks
	}

	ctx, cancel := context.WithCancel(context.Background())

	var xids chan *pluginapi.Device
	if !strings.Contains(disableHealthChecks, "xids") {
		xids = make(chan *pluginapi.Device)
		go watchXIDs(ctx, m.devs, xids)
	}

	for {
		select {
		case <-m.stop:
			cancel()
			return
		case dev := <-xids:
			m.unhealthy(dev)
		}
	}
}

// Serve starts the gRPC server and register the device plugin to Kubelet
func (m *NvidiaDevicePlugin) Serve() error {
	err := m.Start()
	if err != nil {
		log.Printf("Could not start device plugin: %s", err)
		return err
	}
	log.Println("Starting to serve on", m.socket)

	err = m.Register(pluginapi.KubeletSocket, resourceName)
	if err != nil {
		log.Printf("Could not register device plugin: %s", err)
		m.Stop()
		return err
	}
	log.Println("Registered device plugin with Kubelet")

	return nil
}

func getDeviceById(devices []*pluginapi.Device, deviceId string) *pluginapi.Device {
	for _, d := range devices {
		if d.ID == deviceId {
			return d
		}
	}

	return nil
}
