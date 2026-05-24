package main

import (
	"errors"
	"testing"

	"github.com/yumaojun03/dmidecode/parser/baseboard"
	"github.com/yumaojun03/dmidecode/parser/bios"
	"github.com/yumaojun03/dmidecode/parser/memory"
	"github.com/yumaojun03/dmidecode/parser/system"
)

// fakeDMI is a dmiDecoder backed by fixtures. When err is set, every method
// returns it, standing in for a host where DMI cannot be read.
type fakeDMI struct {
	system    []*system.Information
	bios      []*bios.Information
	baseBoard []*baseboard.Information
	memory    []*memory.MemoryDevice
	err       error
}

func (f *fakeDMI) System() ([]*system.Information, error)        { return f.system, f.err }
func (f *fakeDMI) BIOS() ([]*bios.Information, error)            { return f.bios, f.err }
func (f *fakeDMI) BaseBoard() ([]*baseboard.Information, error)  { return f.baseBoard, f.err }
func (f *fakeDMI) MemoryDevice() ([]*memory.MemoryDevice, error) { return f.memory, f.err }

func sampleDMI() *fakeDMI {
	return &fakeDMI{
		system: []*system.Information{{SerialNumber: "SVC123"}},
		bios:   []*bios.Information{{Vendor: "Dell Inc.", BIOSVersion: "2.1.0", ReleaseDate: "01/01/2026"}},
		baseBoard: []*baseboard.Information{
			{Manufacturer: "Dell Inc.", ProductName: "0ABCDE", Version: "A00", SerialNumber: "BB1"},
		},
		memory: []*memory.MemoryDevice{
			{Size: 0}, // empty slot -> skipped
			{Size: 16384, DeviceLocator: "DIMM_A1", Manufacturer: "Micron", PartNumber: "MTA1", SerialNumber: "M1"},
		},
	}
}

func TestBuildInventory(t *testing.T) {
	inv, err := buildInventory(sampleDMI())
	if err != nil {
		t.Fatalf("buildInventory: %v", err)
	}
	if inv.ServiceTag != "SVC123" {
		t.Fatalf("service tag = %q, want SVC123", inv.ServiceTag)
	}
	wantFirmware := Firmware{BIOSVendor: "Dell Inc.", BIOSVersion: "2.1.0", BIOSReleaseDate: "01/01/2026"}
	if inv.Firmware != wantFirmware {
		t.Fatalf("firmware = %+v, want %+v", inv.Firmware, wantFirmware)
	}
	if len(inv.Hardware) != 2 { // baseboard + one populated memory module
		t.Fatalf("hardware items = %d, want 2 (empty slot skipped)", len(inv.Hardware))
	}
	if inv.Hardware[0].Type != "baseboard" || inv.Hardware[0].Product != "0ABCDE" {
		t.Fatalf("baseboard item = %+v", inv.Hardware[0])
	}
	mem := inv.Hardware[1]
	if mem.Type != "memory" || mem.Locator != "DIMM_A1" || mem.Size != "16 GiB" || mem.PartNumber != "MTA1" {
		t.Fatalf("memory item = %+v", mem)
	}
}

func TestBuildInventoryPropagatesErrors(t *testing.T) {
	failing := &fakeDMI{err: errors.New("dmi read failed")}
	if _, err := buildInventory(failing); err == nil {
		t.Fatal("buildInventory should fail when the decoder errors")
	}
	if _, err := inventoryFirmware(failing); err == nil {
		t.Fatal("inventoryFirmware should propagate the decoder error")
	}
	if _, err := inventoryBaseBoards(failing); err == nil {
		t.Fatal("inventoryBaseBoards should propagate the decoder error")
	}
	if _, err := inventoryMemory(failing); err == nil {
		t.Fatal("inventoryMemory should propagate the decoder error")
	}
}

func TestCollectInventory(t *testing.T) {
	prev := openDMIDecoder
	defer func() { openDMIDecoder = prev }()

	openDMIDecoder = func() (dmiDecoder, error) { return nil, errors.New("no dmi here") }
	if _, err := collectInventory(); err == nil {
		t.Fatal("expected error when the decoder cannot be opened")
	}

	openDMIDecoder = func() (dmiDecoder, error) { return sampleDMI(), nil }
	inv, err := collectInventory()
	if err != nil {
		t.Fatalf("collectInventory: %v", err)
	}
	if inv.ServiceTag != "SVC123" {
		t.Fatalf("service tag = %q, want SVC123", inv.ServiceTag)
	}
}

func TestMemorySizeBytes(t *testing.T) {
	const mib = 1024 * 1024
	cases := []struct {
		name      string
		dev       memory.MemoryDevice
		wantBytes uint64
		installed bool
	}{
		{"empty slot", memory.MemoryDevice{Size: 0}, 0, false},
		{"installed unknown size", memory.MemoryDevice{Size: 0xFFFF}, 0, true},
		{"extended size in MiB", memory.MemoryDevice{Size: 0x7FFF, ExtendedSize: 65536}, 65536 * mib, true},
		{"megabyte granularity", memory.MemoryDevice{Size: 8192}, 8192 * mib, true},
		{"kilobyte granularity", memory.MemoryDevice{Size: 0x8400}, mib, true}, // bit15 set, low bits = 1024 KiB
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			gotBytes, gotInstalled := memorySizeBytes(&tt.dev)
			if gotBytes != tt.wantBytes || gotInstalled != tt.installed {
				t.Fatalf("memorySizeBytes = %d, %v; want %d, %v", gotBytes, gotInstalled, tt.wantBytes, tt.installed)
			}
		})
	}
}

func TestHumanMemorySize(t *testing.T) {
	cases := map[uint64]string{
		0:                       "unknown",
		16 * 1024 * 1024 * 1024: "16 GiB",
		512 * 1024 * 1024:       "512 MiB",
	}
	for bytes, want := range cases {
		if got := humanMemorySize(bytes); got != want {
			t.Fatalf("humanMemorySize(%d) = %q, want %q", bytes, got, want)
		}
	}
}
