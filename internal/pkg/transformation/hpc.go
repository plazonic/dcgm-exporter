/*
 * Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package transformation

import (
	"bufio"
	"fmt"
	"log/slog"
	sysOS "os"
	"path"
	"strconv"
	"strings"

	"github.com/NVIDIA/dcgm-exporter/internal/pkg/appconfig"
	"github.com/NVIDIA/dcgm-exporter/internal/pkg/collector"
	"github.com/NVIDIA/dcgm-exporter/internal/pkg/deviceinfo"
	"github.com/NVIDIA/dcgm-exporter/internal/pkg/logging"
	"github.com/NVIDIA/dcgm-exporter/internal/pkg/utils"
	"github.com/sirupsen/logrus"
)

type hpcMapper struct {
	Config *appconfig.Config
}

func newHPCMapper(c *appconfig.Config) *hpcMapper {
	slog.Info(fmt.Sprintf("HPC job mapping is enabled and watch for the %q directory", c.HPCJobMappingDir))
	return &hpcMapper{
		Config: c,
	}
}

func (p *hpcMapper) Name() string {
	return "hpcMapper"
}

func (p *hpcMapper) Process(metrics collector.MetricsByCounter, sysInfo deviceinfo.Provider) error {
	_, err := os.Stat(p.Config.HPCJobMappingDir)
	if err != nil {
		slog.Error(fmt.Sprintf("Unable to access HPC job mapping file directory '%s' - directory not found. Ignoring.",
			p.Config.HPCJobMappingDir), slog.String(logging.ErrorKey, err.Error()))
		return nil
	}

	gpuFiles, err := getGPUFiles(p.Config.HPCJobMappingDir)
	if err != nil {
		return err
	}

	gpuToJobMap := make(map[string][]string)
	// used to find GPU UUIDs from GPU and GPUInstanceID, either GPU-* or MIG-*
	gpuUUIDs := make(map[string]string)

	slog.Debug(fmt.Sprintf("HPC job mapping files: %#v", gpuFiles))

	for _, gpuFileName := range gpuFiles {
		jobs, err := readFile(path.Join(p.Config.HPCJobMappingDir, gpuFileName))
		if err != nil {
			return err
		}

		if _, exist := gpuToJobMap[gpuFileName]; !exist {
			gpuToJobMap[gpuFileName] = []string{}
		}
		gpuToJobMap[gpuFileName] = append(gpuToJobMap[gpuFileName], jobs...)
	}

	slog.Debug(fmt.Sprintf("GPU to job mapping: %+v", gpuToJobMap))

	for counter := range metrics {
		var modifiedMetrics []collector.Metric
		for _, metric := range metrics[counter] {
			var jobs []string
			var exists bool

			if metric.Counter.Multiplier != 1 {
				if strings.Contains(metric.Value, ".") {
					newval, _ := strconv.ParseFloat(metric.Value, 64)
					metric.AlterValue = fmt.Sprintf("%f", newval*float64(metric.Counter.Multiplier))
				} else {
					newval, _ := strconv.Atoi(metric.Value)
					metric.AlterValue = fmt.Sprintf("%d", newval*metric.Counter.Multiplier)
				}
			} else {
				metric.AlterValue = metric.Value
			}
			// either just gpuid (say 2) or if MIG gpuid.gpuinstanceid (say 2.11)
			var gpuID string
			if metric.MigProfile != "" {
				gpuID = metric.GPU + "." + metric.GPUInstanceID
			} else {
				gpuID = metric.GPU
			}
			// for convenience populate UUIDs
			if _, ok := gpuUUIDs[gpuID]; !ok {
				if metric.MigProfile != "" {
					gpuUUIDs[gpuID] = FindMIGUUID(sysInfo, metric.GPU, metric.GPUInstanceID)
				} else {
					gpuUUIDs[gpuID] = metric.GPUUUID
				}
			}
			metric.AlterUUID = gpuUUIDs[gpuID]
			if jobs, exists = gpuToJobMap[gpuUUIDs[gpuID]]; !exists {
				jobs, exists = gpuToJobMap[gpuID]
			}
			if exists && len(jobs) != 0 {
				for _, job := range jobs {
					modifiedMetric, err := utils.DeepCopy(metric)
					if err != nil {
						slog.Error(fmt.Sprintf("Can not create deepCopy for the value: %v", metric),
							slog.String(logging.ErrorKey, err.Error()))
						continue
					}
					if strings.Contains(job, " ") {
						job_user := strings.Split(job, " ")
						if len(job_user) != 2 {
							slog.Error(fmt.Sprintf("Invalid job+user %s for GPU %s", job, metric.GPU))
							continue
						}
						modifiedMetric.Attributes[HpcJobAttribute] = job_user[0]
						modifiedMetric.Attributes[HpcUserAttribute] = job_user[1]
					} else {
						modifiedMetric.Attributes[HpcJobAttribute] = job
					}
					modifiedMetrics = append(modifiedMetrics, modifiedMetric)
				}
			} else {
				modifiedMetrics = append(modifiedMetrics, metric)
			}
		}
		metrics[counter] = modifiedMetrics
	}

	return nil
}

func FindMIGUUID(sysInfo deviceinfo.Provider, gpu string, instanceId string) string {
	gpuidtemp, err := strconv.ParseUint(gpu, 10, 32)
	if err != nil {
		logrus.Fatalf("Got metric with GPU id %s that is not a positive integer", gpu)
	}
	gpuid := uint(gpuidtemp)
	if gpuid >= sysInfo.GPUCount() {
		logrus.Fatalf("Got metric with gpu id %s which is bigger than the number of GPUs %d", gpu, sysInfo.GPUCount())
	}
	migidtemp, err2 := strconv.ParseUint(instanceId, 10, 32)
	if err2 != nil {
		logrus.Fatalf("Got metric for GPU #%s and MIG instance id %s that is not a positive integer", gpu, instanceId)
	}
	migid := uint(migidtemp)
	for j := uint(0); int(j) < len(sysInfo.GPUs()[gpuid].GPUInstances); j++ {
		if sysInfo.GPUs()[gpuid].GPUInstances[j].Info.NvmlInstanceId == migid {
			return sysInfo.GPUs()[gpuid].GPUInstances[j].UUID
		}
	}

	logrus.Warnf("Got metric for GPU #%s and MIG instance id %s that I cannot find in sysinfo.", gpu, instanceId)
	return ""
}

func readFile(path string) ([]string, error) {
	var jobs []string

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func(file *sysOS.File) {
		err := file.Close()
		if err != nil {
			slog.Error(fmt.Sprintf("Failed for close the file: %s", file.Name()),
				slog.String(logging.ErrorKey, err.Error()))
		}
	}(file)

	// Example of the expected file format:
	// job1
	// job2
	// job3
	// or
	// jobid1 uid1
	// jobid2 uid2
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		jobs = append(jobs, scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return jobs, nil
}

func getGPUFiles(dirPath string) ([]string, error) {
	files, err := os.ReadDir(dirPath)
	if err != nil {
		return nil, err
	}

	slog.Debug(fmt.Sprintf("hpc mapper: %d files in the %q found", len(files), dirPath))

	var mappingFiles []string

	for _, file := range files {
		finfo, err := file.Info()
		if err != nil {
			slog.Warn(fmt.Sprintf("HPC mapper: can not get file info for the %s file.", file.Name()))
			continue // Skip files that we can't read
		}

		if finfo.IsDir() {
			slog.Debug(fmt.Sprintf("HPC mapper: the %q file is directory", file.Name()))
			continue // Skip directories
		}

		mappingFiles = append(mappingFiles, file.Name())
	}

	return mappingFiles, nil
}
