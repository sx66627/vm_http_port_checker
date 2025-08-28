package main

import (
    "flag"
    "fmt"
    "io"
    "log"
    "net"
    "net/http"
    "os"
    "strings"
    "sync"
    "time"

    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promhttp"
    "gopkg.in/yaml.v3"
    "crypto/tls"
)

// Флаги
var (
    port                 = flag.Int("port", 9133, "Порт, на котором будет слушать exporter")
    configPath           = flag.String("config", "config.yaml", "Путь к конфигурационному файлу")
    configURL            = flag.String("config-url", "", "URL для загрузки конфига (опционально)")
    configRefreshMinutes = flag.Int("config-refresh-minutes", 60, "Интервал обновления конфига из URL (в минутах)")
)

// Типы
type Config struct {
    TimeoutSeconds       float64  `yaml:"timeout_seconds"`
    CheckIntervalSeconds float64  `yaml:"check_interval_seconds"`
    Targets              []Target `yaml:"targets"`
}

type Target struct {
    Name           string   `yaml:"name"`
    Address        string   `yaml:"address"`
    TimeoutSeconds *float64 `yaml:"timeout_seconds,omitempty"`
}

func (t *Target) GetTimeout(global float64) time.Duration {
    if t.TimeoutSeconds != nil {
	return time.Duration(*t.TimeoutSeconds) * time.Second
    }
    return time.Duration(global) * time.Second
}

type Result struct {
    Success     bool
    Duration    float64
    HTTPStatus  int
    HTTPSize    int64
    ProbeType   string // "http", "https", "tcp"
}

// Глобальные переменные
var (
    results          = make(map[string]Result)
    resultsMu        sync.RWMutex
    configReloadChan = make(chan struct{}, 1) // Сигнал перезагрузки
    schedulerStopChan chan struct{}           // Для остановки старого тикера
)

// Метрики
var (
    probeSuccess = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
	    Name: "probe_success",
	    Help: "Результат проверки хоста (1 = успех, 0 = провал)",
	},
	[]string{"target", "name"},
    )

    probeDuration = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
	    Name: "probe_duration_seconds",
	    Help: "Время выполнения проверки",
	},
	[]string{"target", "name"},
    )

    probeHTTPStatusCode = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
	    Name: "probe_http_status_code",
	    Help: "HTTP статус-код ответа (0 если не HTTP)",
	},
	[]string{"target", "name"},
    )

    probeHTTPResponseSize = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
	    Name: "probe_http_response_size_bytes",
	    Help: "Размер тела HTTP-ответа в байтах (0 если не HTTP)",
	},
	[]string{"target", "name"},
    )

    probeType = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
	    Name: "probe_type",
	    Help: "Тип проверки: http, https, tcp",
	},
	[]string{"target", "name", "type"},
    )
)

func init() {
    prometheus.MustRegister(probeSuccess)
    prometheus.MustRegister(probeDuration)
    prometheus.MustRegister(probeHTTPStatusCode)
    prometheus.MustRegister(probeHTTPResponseSize)
    prometheus.MustRegister(probeType)
}

// Загрузка конфига из файла
func loadConfig() (*Config, error) {
    data, err := os.ReadFile(*configPath)
    if err != nil {
	return nil, fmt.Errorf("не удалось прочитать %s: %w", *configPath, err)
    }

    var config Config
    if err := yaml.Unmarshal(data, &config); err != nil {
	return nil, fmt.Errorf("ошибка парсинга YAML: %w", err)
    }

    if config.TimeoutSeconds == 0 {
	config.TimeoutSeconds = 10
    }
    if config.CheckIntervalSeconds == 0 {
	config.CheckIntervalSeconds = 30
    }

    return &config, nil
}

// Загрузка конфига по URL и сохранение в файл
func downloadConfig(url, filePath string) error {
    client := &http.Client{
	Timeout: 30 * time.Second,
	// Редиректы разрешены по умолчанию
    }

    resp, err := client.Get(url)
    if err != nil {
	return fmt.Errorf("ошибка загрузки конфига по URL: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != 200 {
	return fmt.Errorf("неожиданный статус при загрузке конфига: %d", resp.StatusCode)
    }

    body, err := io.ReadAll(resp.Body)
    if err != nil {
	return fmt.Errorf("не удалось прочитать тело ответа: %w", err)
    }

    if len(body) == 0 {
	return fmt.Errorf("загруженный конфиг пуст")
    }

    // Предварительная валидация: попробуем распарсить
    var testConfig Config
    if err := yaml.Unmarshal(body, &testConfig); err != nil {
	return fmt.Errorf("ошибка парсинга YAML из URL: %w", err)
    }

    // Сохраняем на диск
    err = os.WriteFile(filePath, body, 0644)
    if err != nil {
	return fmt.Errorf("не удалось сохранить конфиг в %s: %w", filePath, err)
    }

    log.Printf("Конфиг успешно загружен и сохранён в %s", filePath)
    return nil
}

// Запуск автообновления конфига по URL
func startConfigReloader() {
    if *configURL == "" {
	log.Println("config-url не задан — фоновая загрузка конфига отключена")
	return
    }

    interval := time.Duration(*configRefreshMinutes) * time.Minute
    log.Printf("Автообновление конфига: каждые %v по URL: %s", interval, *configURL)

    ticker := time.NewTicker(interval)
    go func() {
	// Первая загрузка сразу
	if err := downloadConfig(*configURL, *configPath); err != nil {
	    log.Printf("Не удалось загрузить конфиг при старте: %v", err)
	} else {
	    select {
	    case configReloadChan <- struct{}{}:
	    default:
	    }
	}

	for range ticker.C {
	    if err := downloadConfig(*configURL, *configPath); err != nil {
		log.Printf("Ошибка при автообновлении конфига: %v", err)
		continue
	    }
	    select {
	    case configReloadChan <- struct{}{}:
	    default:
	    }
	}
    }()
}

// Перезапуск планировщика проверок
func restartScheduler(config *Config) {
    // Останавливаем предыдущий тикер
    if schedulerStopChan != nil {
	close(schedulerStopChan)
    }

    schedulerStopChan = make(chan struct{})

    interval := time.Duration(config.CheckIntervalSeconds) * time.Second
    ticker := time.NewTicker(interval)

    go func() {
	// Первая проверка
	for _, target := range config.Targets {
	    go runProbe(target, target.GetTimeout(config.TimeoutSeconds))
	}

	for {
	    select {
	    case <-ticker.C:
		for _, target := range config.Targets {
		    go runProbe(target, target.GetTimeout(config.TimeoutSeconds))
		}
	    case <-schedulerStopChan:
		ticker.Stop()
		return
	    }
	}
    }()
}

// Определение типа проверки
func detectProbeType(address string) string {
    switch {
    case strings.HasPrefix(address, "https://"):
	return "https"
    case strings.HasPrefix(address, "http://"):
	return "http"
    default:
	return "tcp"
    }
}

// Проверки
func probeHTTP(url string, timeout time.Duration) (bool, float64, int, int64) {
    tr := &http.Transport{
	TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
    }
    client := &http.Client{
	Timeout:   timeout,
	Transport: tr,
    }

    start := time.Now()
    resp, err := client.Get(url)
    duration := time.Since(start).Seconds()

    if err != nil {
	log.Printf("HTTP запрос к %s не удался: %v", url, err)
	return false, duration, 0, 0
    }
    defer resp.Body.Close()

    body, err := io.ReadAll(resp.Body)
    size := int64(0)
    if err == nil {
	size = int64(len(body))
    }

    return true, duration, resp.StatusCode, size
}

func probeTCP(address string, timeout time.Duration) (bool, float64) {
    start := time.Now()
    conn, err := net.DialTimeout("tcp", address, timeout)
    duration := time.Since(start).Seconds()

    if err != nil {
	log.Printf("TCP подключение к %s не удалось: %v", address, err)
	return false, duration
    }
    conn.Close()
    return true, duration
}

// Запуск проверки
func runProbe(target Target, timeout time.Duration) {
    var success bool
    var duration float64
    var httpStatus int
    var httpSize int64
    var probeTypeStr string

    address := strings.TrimSpace(target.Address)

    switch {
    case strings.HasPrefix(address, "https://"):
	success, duration, httpStatus, httpSize = probeHTTP(address, timeout)
	probeTypeStr = "https"
    case strings.HasPrefix(address, "http://"):
	success, duration, httpStatus, httpSize = probeHTTP(address, timeout)
	probeTypeStr = "http"
    default:
	success, duration = probeTCP(address, timeout)
	httpStatus = 0
	httpSize = 0
	probeTypeStr = "tcp"
    }

    resultsMu.Lock()
    results[target.Address] = Result{
	Success:    success,
	Duration:   duration,
	HTTPStatus: httpStatus,
	HTTPSize:   httpSize,
	ProbeType:  probeTypeStr,
    }
    resultsMu.Unlock()
}

// Обновление метрик
func updateMetricsFromResults(targets []Target) {
    resultsMu.RLock()
    defer resultsMu.RUnlock()

    // Сброс probe_type
    for _, target := range targets {
	for _, t := range []string{"http", "https", "tcp"} {
	    probeType.WithLabelValues(target.Address, target.Name, t).Set(0)
	}
    }

    for _, target := range targets {
	result, exists := results[target.Address]
	if !exists {
	    probeSuccess.WithLabelValues(target.Address, target.Name).Set(0)
	    probeDuration.WithLabelValues(target.Address, target.Name).Set(0)
	    probeHTTPStatusCode.WithLabelValues(target.Address, target.Name).Set(0)
	    probeHTTPResponseSize.WithLabelValues(target.Address, target.Name).Set(0)

	    pt := detectProbeType(target.Address)
	    probeType.WithLabelValues(target.Address, target.Name, pt).Set(1)
	    continue
	}

	var successVal float64
	if result.Success {
	    successVal = 1.0
	} else {
	    successVal = 0.0
	}

	probeSuccess.WithLabelValues(target.Address, target.Name).Set(successVal)
	probeDuration.WithLabelValues(target.Address, target.Name).Set(result.Duration)
	probeHTTPStatusCode.WithLabelValues(target.Address, target.Name).Set(float64(result.HTTPStatus))
	probeHTTPResponseSize.WithLabelValues(target.Address, target.Name).Set(float64(result.HTTPSize))
	probeType.WithLabelValues(target.Address, target.Name, result.ProbeType).Set(1)
    }
}

// HTTP-сервер
func startServer(config *Config) {
    http.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
	updateMetricsFromResults(config.Targets)
	promhttp.Handler().ServeHTTP(w, r)
    })

    http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write([]byte(`
<html>
<head><title>VictoriaMetrics Exporter</title></head>
<body>
<h1>VictoriaMetrics Exporter</h1>
<p><a href="/metrics">Метрики</a></p>
</body>
</html>`))
    })

    addr := fmt.Sprintf(":%d", *port)
    log.Printf("Запуск сервера на порту %s", addr)
    log.Fatal(http.ListenAndServe(addr, nil))
}

// Основная функция
func main() {
    flag.Parse()

    // Запускаем автообновление конфига
    startConfigReloader()

    // Загружаем начальный конфиг
    config, err := loadConfig()
    if err != nil {
	log.Fatal("Ошибка загрузки конфига: ", err)
    }

    if len(config.Targets) == 0 {
	log.Fatal("Нет целей в конфиге")
    }

    log.Printf("Загружено %d целей. Проверка каждые %.0f секунд", len(config.Targets), config.CheckIntervalSeconds)

    // Перезапускаем планировщик
    restartScheduler(config)

    // Горутина для перезагрузки конфига
    go func() {
	for range configReloadChan {
	    log.Println("🔄 Получен сигнал перезагрузки конфига...")
	    newConfig, err := loadConfig()
	    if err != nil {
		log.Printf("❌ Ошибка перезагрузки конфига: %v", err)
		continue
	    }

	    if len(newConfig.Targets) == 0 {
		log.Printf("⚠️ Перезагруженный конфиг пуст или не содержит целей")
		continue
	    }

	    resultsMu.Lock()
	    config = newConfig
	    results = make(map[string]Result) // сбрасываем старые результаты
	    resultsMu.Unlock()

	    log.Printf("✅ Конфиг успешно перезагружен: %d целей", len(config.Targets))
	    restartScheduler(config) // перезапускаем с новым интервалом
	}
    }()

    // Запускаем HTTP-сервер
    startServer(config)
}