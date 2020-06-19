// Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.

package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	// Bin : Name of the binary
	Bin = "gpu-feature-discovery"
)

var (
	// Version : Version of the binary
	// This will be set using ldflags at compile time
	Version = ""
	// MachineTypePath : Path to the file describing the machine type
	// This will be override during unit testing
	MachineTypePath = "/sys/class/dmi/id/product_name"
)

func main() {

	log.SetPrefix(Bin + ": ")

	if Version == "" {
		log.Print("Version is not set.")
		log.Fatal("Be sure to compile with '-ldflags \"-X main.Version=${GFD_VERSION}\"' and to set $GFD_VERSION")
	}

	log.Printf("Running %s in version %s", Bin, Version)

	nvmlLib := NvmlLib{}

	conf := Conf{}
	conf.getConfFromArgv(os.Args)
	conf.getConfFromEnv()
	log.Print("Loaded configuration:")
	log.Print("Oneshot: ", conf.Oneshot)
	log.Print("SleepInterval: ", conf.SleepInterval)
	log.Print("OutputFilePath: ", conf.OutputFilePath)

	log.Print("Start running")
	err := run(nvmlLib, conf)
	if err != nil {
		log.Printf("Unexpected error: %v", err)
	}
	log.Print("Exiting")
}

func getArchFamily(computeMajor, computeMinor int) string {
	switch computeMajor {
	case 1:
		return "tesla"
	case 2:
		return "fermi"
	case 3:
		return "kepler"
	case 5:
		return "maxwell"
	case 6:
		return "pascal"
	case 7:
		if computeMinor < 5 {
			return "volta"
		}
		return "turing"
	case 8:
		return "ampere"
	}
	return "undefined"
}

func getMachineType() (string, error) {
	data, err := ioutil.ReadFile(MachineTypePath)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(data)), nil
}

func run(nvmlInterface NvmlInterface, conf Conf) error {

	if err := nvmlInterface.Init(); err != nil {
		log.Printf("Failed to initialize NVML: %s.", err)
		log.Printf("If this is a GPU node, did you set the docker default runtime to `nvidia`?")
		log.Printf("You can check the prerequisites at: https://github.com/NVIDIA/gpu-feature-discovery#prerequisites")
		log.Printf("You can learn how to set the runtime at: https://github.com/NVIDIA/gpu-feature-discovery#quick-start")
		return err
	}

	defer func() {
		err := nvmlInterface.Shutdown()
		if err != nil {
			log.Println("Shutdown of NVML returned:", nvmlInterface.Shutdown())
		}
	}()

	count, err := nvmlInterface.GetDeviceCount()
	if err != nil {
		return fmt.Errorf("Error getting device count: %v", err)
	}

	if count < 1 {
		return fmt.Errorf("Error: no device found on the node")
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	exitChan := make(chan bool)

	go func() {
		select {
		case s := <-sigChan:
			log.Printf("Received signal \"%v\", shutting down.", s)
			exitChan <- true
		}
	}()

L:
	for {
		device, err := nvmlInterface.NewDevice(0)
		if err != nil {
			return fmt.Errorf("Error getting device: %v", err)
		}

		driverVersion, err := nvmlInterface.GetDriverVersion()
		if err != nil {
			return fmt.Errorf("Error getting driver version: %v", err)
		}

		driverVersionSplit := strings.Split(driverVersion, ".")
		if len(driverVersionSplit) > 3 || len(driverVersionSplit) < 2 {
			return fmt.Errorf("Error getting driver version: Version \"%s\" does not match format \"X.Y[.Z]\"", driverVersion)
		}

		driverMajor := driverVersionSplit[0]
		driverMinor := driverVersionSplit[1]
		driverRev := ""
		if len(driverVersionSplit) > 2 {
			driverRev = driverVersionSplit[2]
		}

		cudaMajor, cudaMinor, err := nvmlInterface.GetCudaDriverVersion()
		if err != nil {
			return fmt.Errorf("Error getting cuda driver version: %v", err)
		}

		machineType, err := getMachineType()
		if err != nil {
			return fmt.Errorf("Error getting machine type: %v", err)
		}

		output := new(bytes.Buffer)

		log.Print("Writing labels to output buffer")
		fmt.Fprintf(output, "nvidia.com/gfd.timestamp=%d\n", time.Now().Unix())
		fmt.Fprintf(output, "nvidia.com/cuda.driver.major=%s\n", driverMajor)
		fmt.Fprintf(output, "nvidia.com/cuda.driver.minor=%s\n", driverMinor)
		fmt.Fprintf(output, "nvidia.com/cuda.driver.rev=%s\n", driverRev)
		fmt.Fprintf(output, "nvidia.com/cuda.runtime.major=%d\n", *cudaMajor)
		fmt.Fprintf(output, "nvidia.com/cuda.runtime.minor=%d\n", *cudaMinor)
		fmt.Fprintf(output, "nvidia.com/gpu.machine=%s\n", strings.Replace(machineType, " ", "-", -1))
		fmt.Fprintf(output, "nvidia.com/gpu.count=%d\n", count)
		if device.Model != nil {
			model := strings.Replace(*device.Model, " ", "-", -1)
			fmt.Fprintf(output, "nvidia.com/gpu.product=%s\n", model)
		}
		if device.Memory != nil {
			memory := *device.Memory
			fmt.Fprintf(output, "nvidia.com/gpu.memory=%d\n", memory)
		}
		if device.CudaComputeCapability.Major != nil {
			major := *device.CudaComputeCapability.Major
			minor := *device.CudaComputeCapability.Minor
			family := getArchFamily(major, minor)
			fmt.Fprintf(output, "nvidia.com/gpu.family=%s\n", family)
			fmt.Fprintf(output, "nvidia.com/gpu.compute.major=%d\n", major)
			fmt.Fprintf(output, "nvidia.com/gpu.compute.minor=%d\n", minor)
		}

		err = writeFileAtomically(conf.OutputFilePath, output.Bytes(), 0644)
		if err != nil {
			return fmt.Errorf("Error writing file '%s': %v", conf.OutputFilePath, err)
		}

		if conf.Oneshot {
			break
		}

		log.Print("Sleeping for ", conf.SleepInterval)

		select {
		case <-exitChan:
			break L
		case <-time.After(conf.SleepInterval):
			break
		}
	}

	return nil
}

func writeFileAtomically(path string, contents []byte, perm os.FileMode) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("Failed to retrieve absolute path of output file: %v", err)
	}

	absDir := filepath.Dir(absPath)
	tmpDir := filepath.Join(absDir, "gfd-tmp")

	err = os.Mkdir(tmpDir, os.ModePerm)
	if err != nil && !os.IsExist(err) {
		return fmt.Errorf("Failed to create temporary directory: %v", err)
	}
	defer func() {
		if err != nil {
			os.RemoveAll(tmpDir)
		}
	}()

	tmpFile, err := ioutil.TempFile(tmpDir, "gfd-")
	if err != nil {
		return fmt.Errorf("Fail to create temporary output file: %v", err)
	}
	defer func() {
		if err != nil {
			tmpFile.Close()
			os.Remove(tmpFile.Name())
		}
	}()

	err = ioutil.WriteFile(tmpFile.Name(), contents, perm)
	if err != nil {
		return fmt.Errorf("Error writing temporary file '%v': %v", tmpFile.Name(), err)
	}

	err = os.Rename(tmpFile.Name(), path)
	if err != nil {
		return fmt.Errorf("Error moving temporary file to '%v': %v", path, err)
	}

	err = os.Chmod(path, perm)
	if err != nil {
		return fmt.Errorf("Error setting permissions on '%v': %v", path, err)
	}

	return nil
}
