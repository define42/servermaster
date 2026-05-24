package main

import (
	"fmt"

	"github.com/yumaojun03/dmidecode"
	"github.com/yumaojun03/dmidecode/parser/baseboard"
	"github.com/yumaojun03/dmidecode/parser/bios"
	"github.com/yumaojun03/dmidecode/parser/memory"
	"github.com/yumaojun03/dmidecode/parser/system"
)

// Inventory is the host's hardware inventory, decoded from DMI/SMBIOS.
type Inventory struct {
	ServiceTag string         `json:"service_tag"`
	Firmware   Firmware       `json:"firmware"`
	Hardware   []HardwareItem `json:"hardware"`
}

type Firmware struct {
	BIOSVendor      string `json:"bios_vendor"`
	BIOSVersion     string `json:"bios_version"`
	BIOSReleaseDate string `json:"bios_release_date"`
}

type HardwareItem struct {
	Type            string  `json:"type"`
	Manufacturer    string  `json:"manufacturer,omitempty"`
	Product         string  `json:"product,omitempty"`
	Version         string  `json:"version,omitempty"`
	SerialNumber    string  `json:"serial_number,omitempty"`
	Locator         string  `json:"locator,omitempty"`
	Size            string  `json:"size,omitempty"`
	PartNumber      string  `json:"part_number,omitempty"`
	FirmwareVersion *string `json:"firmware_version"`
}

// dmiDecoder is the subset of *dmidecode.Decoder used to build the inventory,
// extracted as an interface so tests can supply fixtures without DMI access.
type dmiDecoder interface {
	System() ([]*system.Information, error)
	BIOS() ([]*bios.Information, error)
	BaseBoard() ([]*baseboard.Information, error)
	MemoryDevice() ([]*memory.MemoryDevice, error)
}

// openDMIDecoder opens the host's DMI tables. It is a variable so tests can
// inject a decoder backed by fixtures instead of real (root-only) SMBIOS data.
//
//nolint:gochecknoglobals // injectable seam so the hardware inventory can be tested without DMI/root.
var openDMIDecoder = func() (dmiDecoder, error) { return dmidecode.New() }

// collectInventory decodes the host's DMI/SMBIOS data into a hardware inventory.
// Reading DMI requires root and a host that exposes SMBIOS, so this returns an
// error on machines (or containers) where that is unavailable.
func collectInventory() (*Inventory, error) {
	decoder, err := openDMIDecoder()
	if err != nil {
		return nil, err
	}
	return buildInventory(decoder)
}

func buildInventory(d dmiDecoder) (*Inventory, error) {
	inv := &Inventory{Hardware: []HardwareItem{}}

	tag, err := inventoryServiceTag(d)
	if err != nil {
		return nil, err
	}
	inv.ServiceTag = tag

	firmware, err := inventoryFirmware(d)
	if err != nil {
		return nil, err
	}
	inv.Firmware = firmware

	boards, err := inventoryBaseBoards(d)
	if err != nil {
		return nil, err
	}
	inv.Hardware = append(inv.Hardware, boards...)

	modules, err := inventoryMemory(d)
	if err != nil {
		return nil, err
	}
	inv.Hardware = append(inv.Hardware, modules...)

	return inv, nil
}

func inventoryServiceTag(d dmiDecoder) (string, error) {
	systems, err := d.System()
	if err != nil {
		return "", fmt.Errorf("read system info: %w", err)
	}
	tag := ""
	for _, s := range systems {
		tag = s.SerialNumber
	}
	return tag, nil
}

func inventoryFirmware(d dmiDecoder) (Firmware, error) {
	infos, err := d.BIOS()
	if err != nil {
		return Firmware{}, fmt.Errorf("read bios info: %w", err)
	}
	var firmware Firmware
	for _, b := range infos {
		firmware = Firmware{
			BIOSVendor:      b.Vendor,
			BIOSVersion:     b.BIOSVersion,
			BIOSReleaseDate: b.ReleaseDate,
		}
	}
	return firmware, nil
}

func inventoryBaseBoards(d dmiDecoder) ([]HardwareItem, error) {
	boards, err := d.BaseBoard()
	if err != nil {
		return nil, fmt.Errorf("read baseboard info: %w", err)
	}
	items := make([]HardwareItem, 0, len(boards))
	for _, bb := range boards {
		items = append(items, HardwareItem{
			Type:         "baseboard",
			Manufacturer: bb.Manufacturer,
			Product:      bb.ProductName,
			Version:      bb.Version,
			SerialNumber: bb.SerialNumber,
		})
	}
	return items, nil
}

func inventoryMemory(d dmiDecoder) ([]HardwareItem, error) {
	devices, err := d.MemoryDevice()
	if err != nil {
		return nil, fmt.Errorf("read memory info: %w", err)
	}
	var items []HardwareItem
	for _, m := range devices {
		size, installed := memorySizeBytes(m)
		if !installed {
			continue
		}
		items = append(items, HardwareItem{
			Type:         "memory",
			Locator:      m.DeviceLocator,
			Size:         humanMemorySize(size),
			Manufacturer: m.Manufacturer,
			PartNumber:   m.PartNumber,
			SerialNumber: m.SerialNumber,
		})
	}
	return items, nil
}

// memorySizeBytes interprets a memory device's SMBIOS size field, returning the
// size in bytes and whether a module is installed. Per the SMBIOS spec, 0 means
// the slot is empty, 0xFFFF means installed but of unknown size, 0x7FFF defers
// to ExtendedSize (in MiB), and otherwise bit 15 selects KiB vs MiB granularity.
func memorySizeBytes(m *memory.MemoryDevice) (uint64, bool) {
	const mib = 1024 * 1024
	switch m.Size {
	case 0:
		return 0, false
	case 0xFFFF:
		return 0, true
	case 0x7FFF:
		return uint64(m.ExtendedSize) * mib, true
	default:
		if m.Size&0x8000 != 0 {
			return uint64(m.Size&0x7FFF) * 1024, true
		}
		return uint64(m.Size) * mib, true
	}
}

// humanMemorySize renders a byte count as GiB when whole, otherwise MiB; a zero
// size (installed but of unknown size) is reported as "unknown".
func humanMemorySize(bytes uint64) string {
	const (
		gib = 1024 * 1024 * 1024
		mib = 1024 * 1024
	)
	switch {
	case bytes == 0:
		return "unknown"
	case bytes%gib == 0:
		return fmt.Sprintf("%d GiB", bytes/gib)
	default:
		return fmt.Sprintf("%d MiB", bytes/mib)
	}
}
