package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

type GPU struct {
	Index              int    `json:"index"`
	Name               string `json:"name"`
	MemoryUsedMiB      int64  `json:"memory_used_mib"`
	MemoryTotalMiB     int64  `json:"memory_total_mib"`
	UtilizationPercent int    `json:"utilization_percent"`
}

type GPUProcess struct {
	PID     int    `json:"pid"`
	Name    string `json:"name"`
	UsedMiB int64  `json:"used_mib"`
	AgentID string `json:"agent_id,omitempty"`
	JobID   string `json:"job_id,omitempty"`
}

type LocalModel struct {
	Provider  string    `json:"provider"`
	Name      string    `json:"name"`
	SizeBytes int64     `json:"size_bytes"`
	VRAMBytes int64     `json:"vram_bytes"`
	ExpiresAt time.Time `json:"expires_at"`
}

var runNvidiaSMI = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "nvidia-smi", args...).Output()
}

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

var ollamaClient httpDoer = &http.Client{Timeout: time.Second}

func parseGPUCSV(data []byte) ([]GPU, error) {
	rows, err := csv.NewReader(strings.NewReader(string(data))).ReadAll()
	if err != nil {
		return nil, err
	}
	gpus := make([]GPU, 0, len(rows))
	for _, row := range rows {
		if len(row) != 5 {
			return nil, fmt.Errorf("GPU row has %d fields", len(row))
		}
		index, err := csvNumber(row[0])
		if err != nil {
			return nil, err
		}
		used, err := csvNumber(row[2])
		if err != nil {
			return nil, err
		}
		total, err := csvNumber(row[3])
		if err != nil {
			return nil, err
		}
		util, err := csvNumber(row[4])
		if err != nil {
			return nil, err
		}
		gpus = append(gpus, GPU{Index: int(index), Name: strings.TrimSpace(row[1]), MemoryUsedMiB: used, MemoryTotalMiB: total, UtilizationPercent: int(util)})
	}
	return gpus, nil
}

func parseGPUProcessesCSV(data []byte) ([]GPUProcess, error) {
	rows, err := csv.NewReader(strings.NewReader(string(data))).ReadAll()
	if err != nil {
		return nil, err
	}
	processes := make([]GPUProcess, 0, len(rows))
	for _, row := range rows {
		if len(row) != 3 {
			return nil, fmt.Errorf("GPU process row has %d fields", len(row))
		}
		pid, err := csvNumber(row[0])
		if err != nil {
			return nil, err
		}
		used, err := csvNumber(row[2])
		if err != nil {
			return nil, err
		}
		if pid <= 0 { // A process without a PID cannot be associated with a host process.
			continue
		}
		processes = append(processes, GPUProcess{PID: int(pid), Name: strings.TrimSpace(row[1]), UsedMiB: used})
	}
	return processes, nil
}

func csvNumber(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "[N/A]" || value == "N/A" {
		return 0, nil
	}
	return strconv.ParseInt(value, 10, 64)
}

func ollamaModels(ctx context.Context, client httpDoer) ([]LocalModel, error) {
	host := os.Getenv("OLLAMA_HOST")
	if host == "" {
		host = "http://127.0.0.1:11434"
	} else if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(host, "/")+"/api/ps", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Models []struct {
			Name      string    `json:"name"`
			Size      int64     `json:"size"`
			SizeVRAM  int64     `json:"size_vram"`
			ExpiresAt time.Time `json:"expires_at"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
		return nil, err
	}
	models := make([]LocalModel, 0, len(payload.Models))
	for _, model := range payload.Models {
		models = append(models, LocalModel{Provider: "ollama", Name: model.Name, SizeBytes: model.Size, VRAMBytes: model.SizeVRAM, ExpiresAt: model.ExpiresAt})
	}
	return models, nil
}

func collectGPU(s *Snapshot) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, gpuErr := runNvidiaSMI(ctx, "--query-gpu=index,name,memory.used,memory.total,utilization.gpu", "--format=csv,noheader,nounits")
	if gpuErr == nil {
		var gpus []GPU
		gpus, gpuErr = parseGPUCSV(output)
		if gpuErr == nil {
			s.GPUs = gpus
		}
	}
	// Query compute processes even if GPU metrics were unavailable.
	output, processErr := runNvidiaSMI(ctx, "--query-compute-apps=pid,process_name,used_memory", "--format=csv,noheader,nounits")
	if processErr == nil {
		var processes []GPUProcess
		processes, processErr = parseGPUProcessesCSV(output)
		if processErr == nil {
			s.GPUProcesses = processes
		}
	}
	if gpuErr != nil || processErr != nil {
		detail := ""
		if gpuErr != nil {
			detail = "GPU query: " + gpuErr.Error()
		}
		if processErr != nil {
			if detail != "" {
				detail += "; "
			}
			detail += "process query: " + processErr.Error()
		}
		s.Sources = append(s.Sources, SourceStatus{Name: "nvidia-smi", Detail: detail})
	} else {
		s.Sources = append(s.Sources, SourceStatus{Name: "nvidia-smi", Available: true})
	}
	for i := range s.GPUProcesses {
		p := &s.GPUProcesses[i]
		for _, a := range s.Agents {
			if a.HostPID == p.PID {
				p.AgentID = a.ID
				break
			}
		}
		for _, j := range s.Jobs {
			if j.WorkerPID == p.PID || j.CodexPID == p.PID {
				p.JobID = j.ID
				break
			}
		}
	}
	ollamaCtx, ollamaCancel := context.WithTimeout(context.Background(), time.Second)
	defer ollamaCancel()
	models, err := ollamaModels(ollamaCtx, ollamaClient)
	if err != nil {
		// A stopped Ollama service is an ordinary unavailable source.
		s.Sources = append(s.Sources, SourceStatus{Name: "ollama", Detail: err.Error()})
	} else {
		s.LocalModels = models
		s.Sources = append(s.Sources, SourceStatus{Name: "ollama", Available: true})
	}
}

func renderVRAM(w io.Writer, s Snapshot) {
	fmt.Fprintln(w, "\nVRAM:")
	if len(s.GPUs) == 0 {
		fmt.Fprintln(w, "  no NVIDIA GPU data")
	}
	for _, gpu := range s.GPUs {
		percent := int64(0)
		if gpu.MemoryTotalMiB > 0 {
			percent = gpu.MemoryUsedMiB * 100 / gpu.MemoryTotalMiB
		}
		line := fmt.Sprintf("  GPU %d %s: VRAM %d/%d MiB (%d%%)  util %d%%", gpu.Index, gpu.Name, gpu.MemoryUsedMiB, gpu.MemoryTotalMiB, percent, gpu.UtilizationPercent)
		if percent >= 90 {
			line = "\x1b[1;31m" + line + "  HIGH VRAM\x1b[0m"
		}
		fmt.Fprintln(w, line)
	}
	processes := append([]GPUProcess(nil), s.GPUProcesses...)
	sort.Slice(processes, func(i, j int) bool { return processes[i].UsedMiB > processes[j].UsedMiB })
	if len(processes) > 0 {
		fmt.Fprintln(w, "  Top VRAM processes:")
		for i, p := range processes {
			if i == 5 {
				break
			}
			fmt.Fprintf(w, "    PID %d %s: %d MiB", p.PID, p.Name, p.UsedMiB)
			if p.AgentID != "" {
				fmt.Fprintf(w, "  agent %s", p.AgentID)
			}
			if p.JobID != "" {
				fmt.Fprintf(w, "  job %s", p.JobID)
			}
			fmt.Fprintln(w)
		}
	}
	if len(s.LocalModels) > 0 {
		fmt.Fprintln(w, "  Loaded Ollama models:")
		for _, model := range s.LocalModels {
			fmt.Fprintf(w, "    %s: VRAM %d MiB\n", model.Name, model.VRAMBytes/(1024*1024))
		}
	}
}
