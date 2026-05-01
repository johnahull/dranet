package driver

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"k8s.io/klog/v2"
)

const (
	sysBusPCI      = "/sys/bus/pci"
	vfioPCIDriver  = "vfio-pci"
	devVFIO        = "/dev/vfio"
)

func ensureVFIOModulesLoaded() error {
	for _, mod := range []string{"vfio", "vfio_pci"} {
		if isKernelModuleLoaded(mod) {
			continue
		}
		klog.V(2).Infof("Loading kernel module %s", mod)
		if err := loadKernelModule(mod); err != nil {
			return fmt.Errorf("failed to load module %s: %w", mod, err)
		}
	}
	return nil
}

func isKernelModuleLoaded(name string) bool {
	data, err := os.ReadFile("/proc/modules")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == name {
			return true
		}
	}
	return false
}

func loadKernelModule(name string) error {
	out, err := exec.Command("modprobe", name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("modprobe %s: %s: %w", name, string(out), err)
	}
	return nil
}

func getCurrentDriver(pciAddress string) (string, error) {
	driverLink := filepath.Join(sysBusPCI, "devices", pciAddress, "driver")
	target, err := os.Readlink(driverLink)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return filepath.Base(target), nil
}

func bindVFIOPCI(pciAddress string) (originalDriver string, err error) {
	if err := ensureVFIOModulesLoaded(); err != nil {
		return "", err
	}

	originalDriver, err = getCurrentDriver(pciAddress)
	if err != nil {
		return "", fmt.Errorf("get current driver for %s: %w", pciAddress, err)
	}

	if originalDriver == vfioPCIDriver {
		klog.V(2).Infof("Device %s already bound to vfio-pci", pciAddress)
		return originalDriver, nil
	}

	// Unbind from current driver
	if originalDriver != "" {
		unbindPath := filepath.Join(sysBusPCI, "drivers", originalDriver, "unbind")
		if err := os.WriteFile(unbindPath, []byte(pciAddress), 0200); err != nil {
			return originalDriver, fmt.Errorf("unbind %s from %s: %w", pciAddress, originalDriver, err)
		}
		klog.V(2).Infof("Unbound %s from %s", pciAddress, originalDriver)
	}

	// Set driver_override to vfio-pci
	overridePath := filepath.Join(sysBusPCI, "devices", pciAddress, "driver_override")
	if err := os.WriteFile(overridePath, []byte(vfioPCIDriver), 0200); err != nil {
		return originalDriver, fmt.Errorf("set driver_override for %s: %w", pciAddress, err)
	}

	// Bind to vfio-pci
	bindPath := filepath.Join(sysBusPCI, "drivers", vfioPCIDriver, "bind")
	if err := os.WriteFile(bindPath, []byte(pciAddress), 0200); err != nil {
		if clearErr := os.WriteFile(overridePath, []byte("\x00"), 0200); clearErr != nil {
			klog.Warningf("Failed to clear driver_override for %s after bind failure: %v", pciAddress, clearErr)
		}
		return originalDriver, fmt.Errorf("bind %s to vfio-pci: %w", pciAddress, err)
	}

	// Clear driver_override
	if err := os.WriteFile(overridePath, []byte("\x00"), 0200); err != nil {
		klog.Warningf("Failed to clear driver_override for %s: %v", pciAddress, err)
	}

	klog.V(2).Infof("Bound %s to vfio-pci (was %s)", pciAddress, originalDriver)
	return originalDriver, nil
}

func restoreDriver(pciAddress, originalDriver string) error {
	currentDriver, err := getCurrentDriver(pciAddress)
	if err != nil {
		return fmt.Errorf("get current driver for %s: %w", pciAddress, err)
	}

	if currentDriver == originalDriver {
		return nil
	}

	// Unbind from current driver (vfio-pci)
	if currentDriver != "" {
		unbindPath := filepath.Join(sysBusPCI, "drivers", currentDriver, "unbind")
		if err := os.WriteFile(unbindPath, []byte(pciAddress), 0200); err != nil {
			return fmt.Errorf("unbind %s from %s: %w", pciAddress, currentDriver, err)
		}
	}

	if originalDriver == "" {
		// Trigger driver_probe to re-bind default driver
		probePath := filepath.Join(sysBusPCI, "drivers_probe")
		if err := os.WriteFile(probePath, []byte(pciAddress), 0200); err != nil {
			return fmt.Errorf("trigger drivers_probe for %s: %w", pciAddress, err)
		}
	} else {
		overridePath := filepath.Join(sysBusPCI, "devices", pciAddress, "driver_override")
		if err := os.WriteFile(overridePath, []byte(originalDriver), 0200); err != nil {
			return fmt.Errorf("set driver_override for %s: %w", pciAddress, err)
		}
		bindPath := filepath.Join(sysBusPCI, "drivers", originalDriver, "bind")
		if err := os.WriteFile(bindPath, []byte(pciAddress), 0200); err != nil {
			return fmt.Errorf("bind %s to %s: %w", pciAddress, originalDriver, err)
		}
		if err := os.WriteFile(overridePath, []byte("\x00"), 0200); err != nil {
			klog.Warningf("Failed to clear driver_override for %s: %v", pciAddress, err)
		}
	}

	klog.V(2).Infof("Restored %s to driver %s", pciAddress, originalDriver)
	return nil
}

func getVFIODeviceFile(pciAddress string) (devFileHost, devFileContainer string, err error) {
	iommuGroupLink := filepath.Join(sysBusPCI, "devices", pciAddress, "iommu_group")

	target, err := filepath.EvalSymlinks(iommuGroupLink)
	if err != nil {
		return "", "", fmt.Errorf("resolve iommu_group for %s: %w", pciAddress, err)
	}

	groupNum := filepath.Base(target)
	devFileContainer = filepath.Join(devVFIO, groupNum)
	devFileHost = devFileContainer

	// Check for vfio-noiommu mode
	namePath := filepath.Join(target, "name")
	nameData, err := os.ReadFile(namePath)
	if err == nil {
		name := strings.TrimSpace(string(nameData))
		if name == "vfio-noiommu" {
			devFileHost = filepath.Join(devVFIO, "noiommu-"+groupNum)
			klog.V(2).Infof("Detected vfio-noiommu for %s, host path: %s", pciAddress, devFileHost)
		}
	}

	klog.V(2).Infof("VFIO device for %s: host=%s container=%s (group %s)", pciAddress, devFileHost, devFileContainer, groupNum)
	return devFileHost, devFileContainer, nil
}
