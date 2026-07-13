package service

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/HenZenKuriRIP/k2pay/internal/model"
	"github.com/HenZenKuriRIP/k2pay/internal/util"

	"github.com/shopspring/decimal"
)

// channelHTTPClient 通道服务专用HTTP客户端
var channelHTTPClient = &http.Client{
	Timeout: 30 * time.Second,
}

// ChannelService 支付通道服务（上游易支付转发等）
type ChannelService struct {
	mu sync.RWMutex
}

// ChannelType 通道类型
type ChannelType string

const (
	ChannelTypeLocal ChannelType = "local" // 本地区块链/个人码
	ChannelTypeEpay  ChannelType = "epay"  // 上游彩虹易支付
)

// ChannelConfig 通道配置
type ChannelConfig struct {
	Type      ChannelType `json:"type"`
	Name      string      `json:"name"`
	ApiURL    string      `json:"api_url"`
	Key       string      `json:"key"`
	PayID     string      `json:"pay_id"` // 上游商户 pid
	NotifyKey string      `json:"notify_key"`
	Enabled   bool        `json:"enabled"`
	Priority  int         `json:"priority"`
}

// ChannelOrderResult 通道订单结果
type ChannelOrderResult struct {
	Success    bool            `json:"success"`
	OrderID    string          `json:"order_id"`
	PayURL     string          `json:"pay_url"`
	QRCode     string          `json:"qr_code"`
	Amount     decimal.Decimal `json:"amount"`
	ExpireTime time.Time       `json:"expire_time"`
	Error      string          `json:"error"`
}

var channelService *ChannelService
var channelOnce sync.Once

// GetChannelService 获取通道服务单例
func GetChannelService() *ChannelService {
	channelOnce.Do(func() {
		channelService = &ChannelService{}
	})
	return channelService
}

// GetChannelConfig 获取指定类型的通道配置
func (s *ChannelService) GetChannelConfig(channelType ChannelType) (*ChannelConfig, error) {
	var configs []model.SystemConfig
	model.GetDB().Where(`"key" LIKE 'channel_%'`).Find(&configs)

	configMap := make(map[string]string)
	for _, cfg := range configs {
		configMap[cfg.Key] = cfg.Value
	}

	prefix := fmt.Sprintf("channel_%s_", channelType)
	if configMap[prefix+"enabled"] != "1" {
		return nil, fmt.Errorf("channel %s is not enabled", channelType)
	}

	priority, _ := strconv.Atoi(configMap[prefix+"priority"])

	return &ChannelConfig{
		Type:      channelType,
		Name:      configMap[prefix+"name"],
		ApiURL:    configMap[prefix+"api_url"],
		Key:       configMap[prefix+"key"],
		PayID:     configMap[prefix+"pay_id"],
		NotifyKey: configMap[prefix+"notify_key"],
		Enabled:   true,
		Priority:  priority,
	}, nil
}

// GetEnabledChannels 获取所有启用的通道
func (s *ChannelService) GetEnabledChannels() []*ChannelConfig {
	var configs []model.SystemConfig
	model.GetDB().Where(`"key" LIKE 'channel_%_enabled' AND "value" = '1'`).Find(&configs)

	var channels []*ChannelConfig
	for _, cfg := range configs {
		// channel_epay_enabled -> epay
		var channelType ChannelType
		if len(cfg.Key) > 16 {
			channelType = ChannelType(cfg.Key[8 : len(cfg.Key)-8])
		}
		if channelType == ChannelTypeLocal {
			continue
		}
		if channel, err := s.GetChannelConfig(channelType); err == nil {
			channels = append(channels, channel)
		}
	}
	return channels
}

// CreateEpayOrder 在上游彩虹易支付创建订单
func (s *ChannelService) CreateEpayOrder(cfg *ChannelConfig, order *model.Order, notifyURL string) (*ChannelOrderResult, error) {
	params := map[string]string{
		"pid":          cfg.PayID,
		"type":         util.ToEpayType(order.Type),
		"out_trade_no": order.TradeNo,
		"notify_url":   notifyURL,
		"name":         order.Name,
		"money":        order.Money.StringFixed(2),
	}
	if order.ReturnURL != "" {
		params["return_url"] = order.ReturnURL
	}

	sign := util.GenerateSign(params, cfg.Key)
	params["sign"] = sign
	params["sign_type"] = "MD5"

	urlParams := url.Values{}
	for k, v := range params {
		urlParams.Set(k, v)
	}
	reqURL := stringsTrimSlash(cfg.ApiURL) + "/mapi.php?" + urlParams.Encode()

	resp, err := channelHTTPClient.Get(reqURL)
	if err != nil {
		return nil, fmt.Errorf("request epay failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read epay response failed: %w", err)
	}

	var result struct {
		Code    int    `json:"code"`
		Msg     string `json:"msg"`
		TradeNo string `json:"trade_no"`
		PayURL  string `json:"payurl"`
		PayURL2 string `json:"pay_url"`
		QRCode  string `json:"qrcode"`
		Money   string `json:"money"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse epay response failed: %w", err)
	}
	if result.Code != 1 && result.Code != 0 {
		return &ChannelOrderResult{Success: false, Error: result.Msg}, nil
	}

	payURL := result.PayURL
	if payURL == "" {
		payURL = result.PayURL2
	}
	amount, _ := decimal.NewFromString(result.Money)
	if amount.IsZero() {
		amount = order.Money
	}

	return &ChannelOrderResult{
		Success: true,
		OrderID: result.TradeNo,
		PayURL:  payURL,
		QRCode:  result.QRCode,
		Amount:  amount,
	}, nil
}

// VerifyEpayNotify 验证上游易支付回调
func (s *ChannelService) VerifyEpayNotify(cfg *ChannelConfig, params map[string]string, sign string) bool {
	return util.VerifySign(params, cfg.Key, sign)
}

// HandleUpstreamNotify 处理上游通知
func (s *ChannelService) HandleUpstreamNotify(channelType ChannelType, localTradeNo string, upstreamOrderID string, amount decimal.Decimal) error {
	var order model.Order
	if err := model.GetDB().Where("trade_no = ?", localTradeNo).First(&order).Error; err != nil {
		return fmt.Errorf("order not found: %s", localTradeNo)
	}
	if order.Status != model.OrderStatusPending {
		log.Printf("Order %s already processed, status: %d", localTradeNo, order.Status)
		return nil
	}

	now := time.Now()
	updates := map[string]interface{}{
		"status":           model.OrderStatusPaid,
		"actual_amount":    amount,
		"paid_at":          &now,
		"channel_order_id": upstreamOrderID,
		"api_trade_no":     upstreamOrderID,
	}
	result := model.GetDB().Model(&order).
		Where("status = ?", model.OrderStatusPending).
		Updates(updates)
	if result.Error != nil {
		return fmt.Errorf("update order failed: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return nil
	}

	model.GetDB().First(&order, order.ID)
	log.Printf("Order %s paid via upstream channel %s, amount: %s", localTradeNo, channelType, amount)

	if err := GetOrderService().CreditMerchantBalance(&order); err != nil {
		log.Printf("Order %s credit balance failed after upstream pay: %v", localTradeNo, err)
	}
	go GetNotifyService().NotifyOrder(order.ID)
	go GetTelegramService().NotifyOrderPaid(&order)
	return nil
}

func stringsTrimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}


