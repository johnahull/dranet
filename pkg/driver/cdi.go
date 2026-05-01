package driver

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"k8s.io/klog/v2"
)

const (
	cdiVendor  = "dra.net"
	cdiClass   = "vfio"
	cdiVersion = "0.8.0"
)

type cdiSpec struct {
	CDIVersion     string            `json:"cdiVersion"`
	Kind           string            `json:"kind"`
	Devices        []cdiDevice       `json:"devices"`
	ContainerEdits cdiContainerEdits `json:"containerEdits"`
}

type cdiDevice struct {
	Name           string            `json:"name"`
	ContainerEdits cdiContainerEdits `json:"containerEdits"`
}

type cdiContainerEdits struct {
	DeviceNodes []cdiDeviceNode `json:"deviceNodes,omitempty"`
}

type cdiDeviceNode struct {
	Path     string `json:"path"`
	HostPath string `json:"hostPath,omitempty"`
	Type     string `json:"type,omitempty"`
}

type cdiManager struct {
	rootPath string
	mu       sync.Mutex
}

func newCDIManager(rootPath string) *cdiManager {
	return &cdiManager{rootPath: rootPath}
}

func (m *cdiManager) specFileName(claimUID string) string {
	return filepath.Join(m.rootPath, fmt.Sprintf("%s-%s-%s.json", cdiVendor, cdiClass, claimUID))
}

func (m *cdiManager) deviceID(claimUID, deviceName string) string {
	return fmt.Sprintf("%s/%s=%s-%s", cdiVendor, cdiClass, claimUID, deviceName)
}

func (m *cdiManager) CreateVFIOSpec(claimUID, deviceName string, vfioCfg *VFIOConfig) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	devID := fmt.Sprintf("%s-%s", claimUID, deviceName)

	spec := cdiSpec{
		CDIVersion: cdiVersion,
		Kind:       fmt.Sprintf("%s/%s", cdiVendor, cdiClass),
		Devices: []cdiDevice{
			{
				Name: devID,
				ContainerEdits: cdiContainerEdits{
					DeviceNodes: []cdiDeviceNode{
						{
							Path:     vfioCfg.VFIOContainerDevPath,
							HostPath: vfioCfg.VFIOGroupDevPath,
							Type:     "c",
						},
						{
							Path:     "/dev/vfio/vfio",
							HostPath: "/dev/vfio/vfio",
							Type:     "c",
						},
					},
				},
			},
		},
	}

	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal CDI spec: %w", err)
	}

	specFile := m.specFileName(claimUID)
	if err := os.MkdirAll(filepath.Dir(specFile), 0755); err != nil {
		return "", fmt.Errorf("create CDI directory: %w", err)
	}

	if err := os.WriteFile(specFile, data, 0644); err != nil {
		return "", fmt.Errorf("write CDI spec %s: %w", specFile, err)
	}

	qualifiedID := m.deviceID(claimUID, deviceName)
	klog.V(2).Infof("Created CDI spec %s with device %s", specFile, qualifiedID)
	return qualifiedID, nil
}

func (m *cdiManager) DeleteSpec(claimUID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	specFile := m.specFileName(claimUID)
	if err := os.Remove(specFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove CDI spec %s: %w", specFile, err)
	}
	klog.V(4).Infof("Deleted CDI spec %s", specFile)
	return nil
}
