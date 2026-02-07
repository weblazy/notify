package main

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/weblazy/easy/econfig"
	"github.com/weblazy/easy/http/http_client"
	"github.com/weblazy/easy/http/http_client/http_client_config"
)

var (
	httpClient   *http_client.HttpClient
	config       *Config
	priceHistory *PriceHistory
)

// ===== 配置结构 =====

// Config 应用配置
type Config struct {
	API struct {
		BinanceURL          string `toml:"BinanceUrl"`
		BinanceKlineURL     string `toml:"BinanceKlineUrl"`
		BybitURL            string `toml:"BybitUrl"`
		BybitKlineURL       string `toml:"BybitKlineUrl"`
		CoinbaseURLTemplate string `toml:"CoinbaseUrlTemplate"`
		KrakenURLTemplate   string `toml:"KrakenUrlTemplate"`
	} `toml:"Api"`
	Notify struct {
		NtfyTopic string `toml:"NtfyTopic"`
		NtfyURL   string `toml:"NtfyUrl"`
	} `toml:"Notify"`
	Monitor struct {
		CheckIntervalSeconds int `toml:"CheckIntervalSeconds"`
		AlertCooldownMinutes int `toml:"AlertCooldownMinutes"`
	} `toml:"Monitor"`
	PriceAlertRules  []PriceAlertRule  `toml:"PriceAlertRules"`
	ChangeAlertRules []ChangeAlertRule `toml:"ChangeAlertRules"`
}

// ===== 数据结构 =====

// ComparisonType 比较类型
type ComparisonType string

const (
	Below ComparisonType = "<" // 低于阈值告警
	Above ComparisonType = ">" // 高于阈值告警
)

// PriceAlertRule 价格告警规则
type PriceAlertRule struct {
	Symbol     string         `toml:"Symbol"`     // 币种符号，如 "BTC", "ETH"
	Threshold  float64        `toml:"Threshold"`  // 阈值价格
	Comparison ComparisonType `toml:"Comparison"` // 比较类型
}

// ChangeAlertRule 涨跌幅告警规则
type ChangeAlertRule struct {
	Symbol        string  `toml:"Symbol"`        // 币种符号，如 "BTC", "ETH"
	ChangePercent float64 `toml:"ChangePercent"` // 涨跌幅阈值（百分比）
	Period        string  `toml:"Period"`        // 时间周期："daily"（当日）或 "15m"（15分钟）
}

// PriceHistory 价格历史记录（仅用于告警冷却）
type PriceHistory struct {
	lastAlertTime  map[string]string // 上次告警时间（按币种+周期组合）
	priceAlertTime time.Time         // 价格告警冷却时间
}

// BinanceKline Binance K线数据
type BinanceKline []interface{}

// BybitKlineResponse Bybit K线响应
type BybitKlineResponse struct {
	Result struct {
		List [][]string `json:"list"`
	} `json:"result"`
	RetCode int    `json:"retCode"`
	RetMsg  string `json:"retMsg"`
}

// BinanceResponse Binance API 响应结构
type BinanceResponse struct {
	Symbol string `json:"symbol"` // 交易对，如 "BTCUSDT"
	Price  string `json:"price"`  // 价格字符串
}

// BybitResponse Bybit API 响应结构
type BybitResponse struct {
	Result struct {
		List []struct {
			Symbol    string `json:"symbol"`
			LastPrice string `json:"lastPrice"`
		} `json:"list"`
	} `json:"result"`
	RetCode int    `json:"retCode"`
	RetMsg  string `json:"retMsg"`
}

// CoinbaseResponse Coinbase API 响应结构
type CoinbaseResponse struct {
	Data struct {
		Amount string `json:"amount"`
	} `json:"data"`
}

// KrakenResponse Kraken API 响应结构
type KrakenResponse struct {
	Result map[string]struct {
		C []string `json:"c"` // [price, volume]
	} `json:"result"`
}

// ===== 价格获取 =====

// getPricesFromBinance 从 Binance 获取所有交易对价格
func getPricesFromBinance() (map[string]float64, error) {
	var tickers []BinanceResponse
	_, err := httpClient.R().
		SetResult(&tickers).
		Get(config.API.BinanceURL)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}

	// 转换为 map
	prices := make(map[string]float64)
	for _, ticker := range tickers {
		var price float64
		if _, err := fmt.Sscanf(ticker.Price, "%f", &price); err == nil {
			prices[ticker.Symbol] = price
		}
	}

	return prices, nil
}

// getPricesFromBybit 从 Bybit 获取所有交易对价格
func getPricesFromBybit() (map[string]float64, error) {
	var result BybitResponse
	_, err := httpClient.R().
		SetResult(&result).
		Get(config.API.BybitURL)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}

	// 转换为 map
	prices := make(map[string]float64)
	for _, ticker := range result.Result.List {
		var price float64
		if _, err := fmt.Sscanf(ticker.LastPrice, "%f", &price); err == nil {
			prices[ticker.Symbol] = price
		}
	}

	return prices, nil
}

// getPricesFromCoinbase 从 Coinbase 获取指定币种价格（仅支持单个获取）
func getPricesFromCoinbase() (map[string]float64, error) {
	prices := make(map[string]float64)

	// 收集所有需要监控的币种
	symbols := make(map[string]bool)
	for _, rule := range config.PriceAlertRules {
		symbols[rule.Symbol] = true
	}
	for _, rule := range config.ChangeAlertRules {
		symbols[rule.Symbol] = true
	}

	// Coinbase 不支持批量获取，需要逐个请求
	for symbol := range symbols {
		url := fmt.Sprintf(config.API.CoinbaseURLTemplate, symbol)
		var result CoinbaseResponse
		_, err := httpClient.R().
			SetResult(&result).
			Get(url)
		if err != nil {
			continue // 跳过失败的币种
		}

		var price float64
		if _, err := fmt.Sscanf(result.Data.Amount, "%f", &price); err == nil {
			// Coinbase 返回的是 USD 价格，近似等于 USDT
			prices[symbol+"USDT"] = price
		}
	}

	if len(prices) == 0 {
		return nil, fmt.Errorf("未获取到任何价格数据")
	}

	return prices, nil
}

// getPricesFromKraken 从 Kraken 获取指定币种价格（仅支持单个获取）
func getPricesFromKraken() (map[string]float64, error) {
	prices := make(map[string]float64)

	// Kraken 币种符号映射
	symbolMap := map[string]string{
		"BTC": "XBTUSD",
		"ETH": "ETHUSD",
		"SOL": "SOLUSD",
	}

	// 收集所有需要监控的币种
	symbols := make(map[string]bool)
	for _, rule := range config.PriceAlertRules {
		symbols[rule.Symbol] = true
	}
	for _, rule := range config.ChangeAlertRules {
		symbols[rule.Symbol] = true
	}

	for symbol := range symbols {
		krakenSymbol, exists := symbolMap[symbol]
		if !exists {
			continue // 跳过不支持的币种
		}

		url := fmt.Sprintf(config.API.KrakenURLTemplate, krakenSymbol)
		var result KrakenResponse
		_, err := httpClient.R().
			SetResult(&result).
			Get(url)
		if err != nil {
			continue
		}

		// Kraken 返回的 key 可能不同，需要遍历
		for _, data := range result.Result {
			if len(data.C) > 0 {
				var price float64
				if _, err := fmt.Sscanf(data.C[0], "%f", &price); err == nil {
					// Kraken 返回的是 USD 价格，近似等于 USDT
					prices[symbol+"USDT"] = price
				}
			}
			break
		}
	}

	if len(prices) == 0 {
		return nil, fmt.Errorf("未获取到任何价格数据")
	}

	return prices, nil
}

// getAllPrices 获取所有交易对价格（多数据源容错）
func getAllPrices() (map[string]float64, string) {
	// 尝试顺序：Binance -> Bybit -> Coinbase -> Kraken
	sources := []struct {
		name string
		fn   func() (map[string]float64, error)
	}{
		{"Binance", getPricesFromBinance},
		{"Bybit", getPricesFromBybit},
		{"Coinbase", getPricesFromCoinbase},
		{"Kraken", getPricesFromKraken},
	}

	for _, source := range sources {
		prices, err := source.fn()
		if err == nil && len(prices) > 0 {
			log.Printf("📡 数据源: %s", source.name)
			return prices, source.name
		}
		log.Printf("⚠️  %s 获取失败: %v，尝试下一个数据源...", source.name, err)
	}

	log.Printf("❌ 所有数据源均失败")
	return nil, ""
}

// ===== K线数据获取 =====

// getDailyOpenPriceFromBinance 从 Binance 获取当日开盘价
func getDailyOpenPriceFromBinance(symbol string) (float64, error) {
	// 获取最近1根日K线
	url := fmt.Sprintf("%s?symbol=%s&interval=1d&limit=1", config.API.BinanceKlineURL, symbol)

	var klines []BinanceKline
	_, err := httpClient.R().
		SetResult(&klines).
		Get(url)
	if err != nil {
		return 0, fmt.Errorf("获取日K线失败: %w", err)
	}

	if len(klines) == 0 {
		return 0, fmt.Errorf("没有K线数据")
	}

	// K线数据格式: [openTime, open, high, low, close, volume, ...]
	// 取开盘价（索引1）
	openPriceStr, ok := klines[0][1].(string)
	if !ok {
		return 0, fmt.Errorf("开盘价格式错误")
	}

	var openPrice float64
	if _, err := fmt.Sscanf(openPriceStr, "%f", &openPrice); err != nil {
		return 0, fmt.Errorf("解析开盘价失败: %w", err)
	}

	return openPrice, nil
}

// getDailyOpenPriceFromBybit 从 Bybit 获取当日开盘价
func getDailyOpenPriceFromBybit(symbol string) (float64, error) {
	// 获取最近1根日K线
	// Bybit 返回数据是倒序的（最新的在前）
	url := fmt.Sprintf("%s?category=spot&symbol=%s&interval=D&limit=1", config.API.BybitKlineURL, symbol)

	var result BybitKlineResponse
	_, err := httpClient.R().
		SetResult(&result).
		Get(url)
	if err != nil {
		return 0, fmt.Errorf("获取日K线失败: %w", err)
	}

	if result.RetCode != 0 {
		return 0, fmt.Errorf("API返回错误: %s", result.RetMsg)
	}

	if len(result.Result.List) == 0 {
		return 0, fmt.Errorf("没有K线数据")
	}

	// Bybit K线格式: [startTime, open, high, low, close, volume, turnover]
	// 取开盘价（索引1）
	openPriceStr := result.Result.List[0][1]
	var openPrice float64
	if _, err := fmt.Sscanf(openPriceStr, "%f", &openPrice); err != nil {
		return 0, fmt.Errorf("解析开盘价失败: %w", err)
	}

	return openPrice, nil
}

// get15mAgoPriceFromBinance 从 Binance 获取15分钟前的价格
func get15mAgoPriceFromBinance(symbol string) (float64, error) {
	// 获取最近2根15分钟K线（第二根是15分钟前的）
	url := fmt.Sprintf("%s?symbol=%s&interval=15m&limit=2", config.API.BinanceKlineURL, symbol)

	var klines []BinanceKline
	_, err := httpClient.R().
		SetResult(&klines).
		Get(url)
	if err != nil {
		return 0, fmt.Errorf("获取15分钟K线失败: %w", err)
	}

	if len(klines) < 2 {
		return 0, fmt.Errorf("K线数据不足")
	}

	// 取第二根K线的收盘价（索引4）
	closePriceStr, ok := klines[1][4].(string)
	if !ok {
		return 0, fmt.Errorf("收盘价格式错误")
	}

	var closePrice float64
	if _, err := fmt.Sscanf(closePriceStr, "%f", &closePrice); err != nil {
		return 0, fmt.Errorf("解析收盘价失败: %w", err)
	}

	return closePrice, nil
}

// get15mAgoPriceFromBybit 从 Bybit 获取15分钟前的价格
func get15mAgoPriceFromBybit(symbol string) (float64, error) {
	// 获取最近2根15分钟K线
	// Bybit 返回数据是倒序的（最新的在前），所以第二根就是15分钟前的
	url := fmt.Sprintf("%s?category=spot&symbol=%s&interval=15&limit=2", config.API.BybitKlineURL, symbol)

	var result BybitKlineResponse
	_, err := httpClient.R().
		SetResult(&result).
		Get(url)
	if err != nil {
		return 0, fmt.Errorf("获取15分钟K线失败: %w", err)
	}

	if result.RetCode != 0 {
		return 0, fmt.Errorf("API返回错误: %s", result.RetMsg)
	}

	if len(result.Result.List) < 2 {
		return 0, fmt.Errorf("K线数据不足")
	}

	// Bybit K线格式: [startTime, open, high, low, close, volume, turnover]
	// 取第二根K线的收盘价（索引4）
	closePriceStr := result.Result.List[1][4]
	var closePrice float64
	if _, err := fmt.Sscanf(closePriceStr, "%f", &closePrice); err != nil {
		return 0, fmt.Errorf("解析收盘价失败: %w", err)
	}

	return closePrice, nil
}

// getHistoricalPriceFromBinance 从 Binance 获取历史价格
func getHistoricalPriceFromBinance(symbol string, period string) (float64, error) {
	if period == "daily" {
		return getDailyOpenPriceFromBinance(symbol)
	} else if period == "15m" {
		return get15mAgoPriceFromBinance(symbol)
	}
	return 0, fmt.Errorf("未知的时间周期: %s", period)
}

// getHistoricalPriceFromBybit 从 Bybit 获取历史价格
func getHistoricalPriceFromBybit(symbol string, period string) (float64, error) {
	if period == "daily" {
		return getDailyOpenPriceFromBybit(symbol)
	} else if period == "15m" {
		return get15mAgoPriceFromBybit(symbol)
	}
	return 0, fmt.Errorf("未知的时间周期: %s", period)
}

// getHistoricalPrice 获取历史价格（从K线数据，支持多数据源容错）
func getHistoricalPrice(symbol string, period string) (float64, error) {
	symbolKey := symbol + "USDT"

	// 数据源列表（按优先级）
	sources := []struct {
		name string
		fn   func(string, string) (float64, error)
	}{
		{"Binance", getHistoricalPriceFromBinance},
		{"Bybit", getHistoricalPriceFromBybit},
	}

	// 尝试各个数据源
	for _, source := range sources {
		price, err := source.fn(symbolKey, period)
		if err == nil && price > 0 {
			return price, nil
		}
		// 静默失败，尝试下一个数据源
	}

	return 0, fmt.Errorf("所有数据源均无法获取 %s %s 历史价格", symbol, period)
}

// ===== 告警检查 =====

// AlertInfo 告警信息
type AlertInfo struct {
	Symbol      string
	AlertType   string  // "price" 或 "change"
	CurrentPrice float64
	Message     string
}

// checkPriceAlerts 检查价格告警规则
func checkPriceAlerts(prices map[string]float64) []AlertInfo {
	var triggered []AlertInfo

	for _, rule := range config.PriceAlertRules {
		// 构建交易对符号（币种 + USDT）
		symbolKey := rule.Symbol + "USDT"

		// 查找价格
		price, exists := prices[symbolKey]
		if !exists {
			log.Printf("⚠️  币种 %s 不存在", symbolKey)
			continue
		}

		// 检查是否触发告警
		var message string
		if rule.Comparison == Below && price < rule.Threshold {
			message = fmt.Sprintf("跌破 $%.2f", rule.Threshold)
		} else if rule.Comparison == Above && price > rule.Threshold {
			message = fmt.Sprintf("突破 $%.2f", rule.Threshold)
		}

		if message != "" {
			triggered = append(triggered, AlertInfo{
				Symbol:       rule.Symbol,
				AlertType:    "price",
				CurrentPrice: price,
				Message:      message,
			})
		}
	}

	return triggered
}

// checkChangeAlerts 检查涨跌幅告警规则
func checkChangeAlerts(prices map[string]float64, alertCooldown time.Duration) []AlertInfo {
	var triggered []AlertInfo
	now := time.Now()

	for _, rule := range config.ChangeAlertRules {
		// 构建交易对符号（币种 + USDT）
		symbolKey := rule.Symbol + "USDT"

		// 查找当前价格
		currentPrice, exists := prices[symbolKey]
		if !exists {
			continue
		}

		// 从API获取历史价格
		basePrice, err := getHistoricalPrice(rule.Symbol, rule.Period)
		if err != nil {
			log.Printf("⚠️  获取 %s %s 历史价格失败: %v", rule.Symbol, rule.Period, err)
			continue
		}

		var periodLabel string
		if rule.Period == "daily" {
			periodLabel = "当日"
		} else if rule.Period == "15m" {
			periodLabel = "15分钟"
		} else {
			periodLabel = rule.Period
		}

		// 计算涨跌幅
		changePercent := (currentPrice - basePrice) / basePrice * 100

		// 检查是否超过阈值
		if abs(changePercent) >= rule.ChangePercent {
			// 构建告警键（币种+周期）
			alertKey := fmt.Sprintf("%s_%s", symbolKey, rule.Period)

			// 检查冷却时间
			lastAlertTimeStr, hasAlert := priceHistory.lastAlertTime[alertKey]
			if hasAlert {
				lastAlertTime, err := time.Parse(time.RFC3339, lastAlertTimeStr)
				if err == nil && now.Sub(lastAlertTime) < alertCooldown {
					continue
				}
			}

			var direction string
			if changePercent > 0 {
				direction = "上涨"
			} else {
				direction = "下跌"
			}

			message := fmt.Sprintf("%s%s %.2f%% (从 $%.2f 到 $%.2f)",
				periodLabel, direction, abs(changePercent), basePrice, currentPrice)

			triggered = append(triggered, AlertInfo{
				Symbol:       rule.Symbol,
				AlertType:    "change",
				CurrentPrice: currentPrice,
				Message:      message,
			})

			// 更新告警时间
			priceHistory.lastAlertTime[alertKey] = now.Format(time.RFC3339)
		}
	}

	return triggered
}

// abs 返回浮点数的绝对值
func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// ===== 告警推送 =====

// sendAlerts 发送告警通知
func sendAlerts(triggered []AlertInfo) error {
	if len(triggered) == 0 {
		return nil
	}

	// 构建消息内容
	var lines []string
	for _, alert := range triggered {
		var emoji string
		if alert.AlertType == "price" {
			emoji = "💰"
		} else {
			emoji = "📈"
		}

		lines = append(lines,
			fmt.Sprintf("%s %s %s\n当前: $%.2f", emoji, alert.Symbol, alert.Message, alert.CurrentPrice))
	}

	message := "🚨 加密货币告警\n\n" + strings.Join(lines, "\n\n")

	// 发送到 ntfy
	_, err := httpClient.R().
		SetHeader("Title", "加密货币告警").
		SetHeader("Priority", "urgent").
		SetHeader("Tags", "warning,chart").
		SetHeader("X-Priority", "5").
		SetBody(message).
		Post(config.Notify.NtfyURL)
	if err != nil {
		return fmt.Errorf("推送失败: %w", err)
	}

	log.Printf("✅ 告警已发送: %d 条", len(triggered))
	return nil
}

// ===== 日志输出 =====

// logCurrentPrices 打印当前监控币种的价格
func logCurrentPrices(prices map[string]float64) {
	// 收集所有需要监控的币种
	symbols := make(map[string]bool)
	for _, rule := range config.PriceAlertRules {
		symbols[rule.Symbol] = true
	}
	for _, rule := range config.ChangeAlertRules {
		symbols[rule.Symbol] = true
	}

	var summary []string
	for symbol := range symbols {
		symbolKey := symbol + "USDT"
		if price, exists := prices[symbolKey]; exists {
			// 尝试获取当日开盘价和15分钟前价格
			dailyOpen, errDaily := getHistoricalPrice(symbol, "daily")
			price15m, err15m := getHistoricalPrice(symbol, "15m")

			if errDaily == nil && dailyOpen > 0 {
				dailyChange := (price - dailyOpen) / dailyOpen * 100
				var arrow string
				if dailyChange > 0 {
					arrow = "↑"
				} else if dailyChange < 0 {
					arrow = "↓"
				} else {
					arrow = "→"
				}

				// 如果有15分钟数据，也显示
				if err15m == nil && price15m > 0 {
					change15m := (price - price15m) / price15m * 100
					summary = append(summary,
						fmt.Sprintf("%s: $%.2f %s%.2f%%(日) %.2f%%(15m)",
							symbol, price, arrow, dailyChange, change15m))
				} else {
					summary = append(summary,
						fmt.Sprintf("%s: $%.2f %s%.2f%%", symbol, price, arrow, dailyChange))
				}
			} else {
				summary = append(summary,
					fmt.Sprintf("%s: $%.2f", symbol, price))
			}
		}
	}
	log.Printf("📊 %s", strings.Join(summary, " | "))
}

// ===== 主监控循环 =====

// monitorCryptoPrices 主监控函数
func monitorCryptoPrices() {
	checkInterval := time.Duration(config.Monitor.CheckIntervalSeconds) * time.Second
	alertCooldown := time.Duration(config.Monitor.AlertCooldownMinutes) * time.Minute

	log.Printf("🚀 开始监控加密货币价格")
	log.Printf("📊 数据源: Binance")
	log.Printf("📱 推送 Topic: %s", config.Notify.NtfyTopic)
	log.Printf("⏱️  检查间隔: %v", checkInterval)

	// 打印监控策略
	log.Printf("💰 价格告警规则:")
	for _, rule := range config.PriceAlertRules {
		op := "低于"
		if rule.Comparison == Above {
			op = "高于"
		}
		log.Printf("  - %s %s $%.2f", rule.Symbol, op, rule.Threshold)
	}

	log.Printf("📈 涨跌幅告警规则:")
	for _, rule := range config.ChangeAlertRules {
		var periodLabel string
		if rule.Period == "daily" {
			periodLabel = "当日"
		} else if rule.Period == "15m" {
			periodLabel = "15分钟"
		} else {
			periodLabel = rule.Period
		}
		log.Printf("  - %s %s涨跌幅超过 %.1f%%", rule.Symbol, periodLabel, rule.ChangePercent)
	}

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	var lastAlertTime time.Time

	for range ticker.C {
		// 1. 获取所有价格
		prices, source := getAllPrices()
		if prices == nil {
			log.Printf("❌ 所有数据源均失败")
			continue
		}
		_ = source // 数据源已在 getAllPrices 中打印

		// 2. 检查价格告警规则
		priceAlerts := checkPriceAlerts(prices)

		// 3. 检查涨跌幅告警规则
		changeAlerts := checkChangeAlerts(prices, alertCooldown)

		// 4. 如果有价格告警且不在冷却期，发送告警
		if len(priceAlerts) > 0 {
			if time.Since(lastAlertTime) > alertCooldown {
				if err := sendAlerts(priceAlerts); err != nil {
					log.Printf("❌ 发送价格告警失败: %v", err)
				} else {
					lastAlertTime = time.Now()
				}
			} else {
				remaining := alertCooldown - time.Since(lastAlertTime)
				log.Printf("⏳ 价格告警冷却中，剩余 %.0f 秒", remaining.Seconds())
			}
		}

		// 6. 涨跌幅告警独立发送（有独立的冷却时间）
		if len(changeAlerts) > 0 {
			if err := sendAlerts(changeAlerts); err != nil {
				log.Printf("❌ 发送涨跌幅告警失败: %v", err)
			}
		}

		// 7. 打印当前价格
		logCurrentPrices(prices)
	}
}

func main() {
	// 初始化配置
	config = &Config{}
	econfig.InitGlobalViper(config)

	// 初始化价格历史记录（仅用于告警冷却）
	priceHistory = &PriceHistory{
		lastAlertTime: make(map[string]string),
	}

	// 初始化 HTTP 客户端
	cfg := http_client_config.DefaultConfig()
	cfg.ReadTimeout = 10 * time.Second
	httpClient = http_client.NewHttpClient(cfg)

	log.Println("=======================================")
	log.Println("  加密货币价格监控服务")
	log.Println("=======================================")
	monitorCryptoPrices()
}
