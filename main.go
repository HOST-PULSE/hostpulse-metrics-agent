package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Синхронизировали структуру с Django Serializer
type MetricsPayload struct {
	ServerToken string  `json:"server_token"` // Передаем токен прямо внутри JSON
	CPUUsage    float64 `json:"cpu_usage"`
	MemUsage    float64 `json:"ram_usage"`    // В Django поле называется ram_usage
	DiskUsage   float64 `json:"disk_usage"`
}

func main() {
	fmt.Println("=== Универсальный Go-агент метрик HostPulse успешно запущен ===")

	// 1. Читаем прямой URL эндпоинта из переменной, которую дает бот
	metricsURL := os.Getenv("HOSTPULSE_METRICS_URL")
	if metricsURL == "" {
		// Резервный откат на ваш дефолтный адрес
		metricsURL = "https://zedform.kz"
	}

	// 2. Читаем токен авторизации хоста
	agentToken := os.Getenv("HOSTPULSE_TOKEN")
	if agentToken == "" {
		fmt.Println(" [⚠️ WARNING] HOSTPULSE_TOKEN не задан. Используется дефолтный токен тестовой среды.")
		agentToken = "default_token_123"
	}

	// 3. Читаем секрет для будущих команд (просто проверяем наличие)
	agentSecret := os.Getenv("HOSTPULSE_SECRET")
	if agentSecret != "" {
		fmt.Println(" [🔒 SECURITY] Секретный ключ авторизации удаленных команд успешно загружен.")
	}

	fmt.Printf(" [INFO] Эндпоинт отправки телеметрии: %s\n", metricsURL)
	fmt.Printf(" [INFO] Идентификатор токена: %s...%s\n", agentToken[:4], agentToken[len(agentToken)-4:])

	// Таймер отправки: 10 секунд (вполне укладывается в наш 30-секундный таймаут в CRM)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	client := &http.Client{Timeout: 5 * time.Second}

	for range ticker.C {
		payload := collectPureGoMetrics()
		// На лету привязываем токен к пакету метрик
		payload.ServerToken = agentToken

		fmt.Printf(" [📊 МЕТРИКИ] CPU: %.1f%% | RAM: %.1f%% | DISK: %.1f%% \n",
			payload.CPUUsage, payload.MemUsage, payload.DiskUsage)

		// Запускаем отправку в горутине, чтобы сетевые задержки не тормозили таймер
		go sendMetrics(client, metricsURL, payload)
	}
}

func collectPureGoMetrics() *MetricsPayload {
	return &MetricsPayload{
		CPUUsage:  getCPUUsage(),
		MemUsage:  getMemoryUsage(),
		DiskUsage: getDiskUsage(),
	}
}

// 1. Получение загрузки CPU через чтение /proc/stat
func getCPUUsage() float64 {
	readStats := func() (idle, total float64) {
		file, err := os.Open("/proc/stat")
		if err != nil {
			return 0, 0
		}
		defer file.Close()

		scanner := bufio.NewScanner(file)
		if scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) > 4 && fields[0] == "cpu" {
				var sum float64
				for i := 1; i < len(fields); i++ {
					val, _ := strconv.ParseFloat(fields[i], 64)
					sum += val
					if i == 4 { // 4-е поле — время простоя
						idle = val
					}
				}
				total = sum
			}
		}
		return
	}

	idle1, total1 := readStats()
	time.Sleep(500 * time.Millisecond) // Дельта за 500 мс
	idle2, total2 := readStats()

	totalDelta := total2 - total1
	idleDelta := idle2 - idle1

	if totalDelta == 0 {
		return 0.0
	}
	return (1.0 - idleDelta/totalDelta) * 100.0
}

// 2. Получение загрузки RAM через чтение /proc/meminfo
func getMemoryUsage() float64 {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0.0
	}
	defer file.Close()

	var memTotal, memAvailable float64
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		if fields[0] == "MemTotal:" {
			memTotal, _ = strconv.ParseFloat(fields[1], 64)
		}
		if fields[0] == "MemAvailable:" {
			memAvailable, _ = strconv.ParseFloat(fields[1], 64)
		}
	}

	if memTotal == 0 {
		return 0.0
	}
	return ((memTotal - memAvailable) / memTotal) * 100.0
}

// 3. Получение загрузки диска через системный вызов Statfs
func getDiskUsage() float64 {
	var stat syscall.Statfs_t
	err := syscall.Statfs("/", &stat)
	if err != nil {
		return 0.0
	}

	allBlocks := float64(stat.Blocks)
	freeBlocks := float64(stat.Bfree)

	if allBlocks == 0 {
		return 0.0
	}
	return ((allBlocks - freeBlocks) / allBlocks) * 100.0
}

// 4. Функция отправки JSON на Django-бэкенд
func sendMetrics(client *http.Client, url string, payload *MetricsPayload) {
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return
	}

	req.Header.Set("X-Agent-Token", payload.ServerToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		// Если CRM временно недоступна, агент просто пропустит этот тик
		return
	}

	// Обязательно вычитываем и закрываем боди, чтобы не вешать дескрипторы системы
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}
