package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HenZenKuriRIP/k2pay/internal/model"
	"github.com/HenZenKuriRIP/k2pay/internal/payment"
	"github.com/HenZenKuriRIP/k2pay/internal/util"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// orderCacheItem 订单缓存项
type orderCacheItem struct {
	order     *model.Order
	paid      bool
	timestamp time.Time
}

// OrderService 订单服务
type OrderService struct {
	cache    sync.Map // 订单状态缓存: tradeNo -> orderCacheItem
	cacheTTL time.Duration
}

var orderService *OrderService

// GetOrderService 获取订单服务
func GetOrderService() *OrderService {
	if orderService == nil {
		orderService = &OrderService{
			cacheTTL: 5 * time.Second, // 缓存5秒
		}
	}
	return orderService
}

// CreateOrderRequest 创建订单请求
type CreateOrderRequest struct {
	MerchantPID string `json:"pid" form:"pid"`
	Type        string `json:"type" form:"type"`
	OutTradeNo  string `json:"out_trade_no" form:"out_trade_no"`
	NotifyURL   string `json:"notify_url" form:"notify_url"`
	ReturnURL   string `json:"return_url" form:"return_url"`
	Name        string `json:"name" form:"name"`
	Money       string `json:"money" form:"money"`
	Currency    string `json:"currency" form:"currency"` // 货币类型: CNY, USD, USDT 等，默认 CNY
	Param       string `json:"param" form:"param"`
	ClientIP    string `json:"-"`
	Channel     string `json:"channel" form:"channel"` // 指定支付通道: local, epay (可选)
}

// CreateOrderResponse 创建订单响应
type CreateOrderResponse struct {
	Code           int    `json:"code"`
	Msg            string `json:"msg"`
	TradeNo        string `json:"trade_no,omitempty"`
	OutTradeNo     string `json:"out_trade_no,omitempty"`
	Type           string `json:"type,omitempty"`
	Currency       string `json:"currency,omitempty"`         // 原始货币
	Money          string `json:"money,omitempty"`            // 原始金额
	PayCurrency    string `json:"pay_currency,omitempty"`     // 支付货币
	PayAmount      string `json:"pay_amount,omitempty"`       // 用户应支付金额（展示用，无偏移）
	UniqueAmount   string `json:"unique_amount,omitempty"`    // 订单标识金额（含偏移，实际支付）
	USDTAmount     string `json:"usdt_amount,omitempty"`      // USDT金额(兼容旧字段，等于 unique_amount)
	Rate           string `json:"rate,omitempty"`
	Address        string `json:"address,omitempty"`
	Chain          string `json:"chain,omitempty"`
	QRCode         string `json:"qrcode,omitempty"`
	ExpiredAt      string `json:"expired_at,omitempty"`
	PayURL         string `json:"pay_url,omitempty"`
	Channel        string `json:"channel,omitempty"`          // 实际使用的支付通道
	ChannelPayURL  string `json:"channel_pay_url,omitempty"`  // 上游支付链接 (如果使用通道)
}

// CreateOrder 创建订单
func (s *OrderService) CreateOrder(req *CreateOrderRequest) (*CreateOrderResponse, error) {
	// 验证商户
	var merchant model.Merchant
	if err := model.GetDB().Where("p_id = ? AND status = 1", req.MerchantPID).First(&merchant).Error; err != nil {
		return nil, errors.New("商户不存在或已禁用")
	}

	// SSRF 防护：校验回调/跳转 URL
	if err := util.ValidateCallbackURL(req.NotifyURL); err != nil {
		return nil, errors.New("notify_url 无效: " + err.Error())
	}
	if err := util.ValidateCallbackURL(req.ReturnURL); err != nil {
		return nil, errors.New("return_url 无效: " + err.Error())
	}

	// 验证金额
	money, err := decimal.NewFromString(req.Money)
	if err != nil || money.LessThanOrEqual(decimal.Zero) {
		return nil, errors.New("金额无效")
	}

	// 标准化货币类型（易支付默认 CNY）
	currency := NormalizeCurrency(req.Currency)
	if currency == "" {
		currency = "CNY"
	}

	// 校验商户订单号格式
	if !util.IsValidOutTradeNo(req.OutTradeNo) {
		return nil, errors.New("订单号(out_trade_no)格式不正确")
	}

	// 支付类型：允许为空（进收银台后选择，经典易支付行为）
	typePending := strings.TrimSpace(req.Type) == ""
	var payType, chain string
	var isFiat bool
	if !typePending {
		meta, err := util.ResolvePaymentType(req.Type)
		if err != nil || meta == nil {
			return nil, errors.New("不支持的支付类型")
		}
		payType = util.NormalizePaymentType(req.Type)
		chain = meta.Chain
		if !util.IsValidChain(chain) {
			return nil, errors.New("不支持的支付类型")
		}
		isFiat = meta.IsFiat
	}

	// 检查订单号是否重复（10 天内未支付订单参数一致则复用）
	var existingOrder model.Order
	err = model.GetDB().Where("merchant_id = ? AND out_trade_no = ?", merchant.ID, req.OutTradeNo).First(&existingOrder).Error
	if err == nil {
		if existingOrder.Status == model.OrderStatusPending {
			// 参数变化则拒绝
			if !existingOrder.Money.Equal(money) || existingOrder.Name != req.Name {
				return nil, errors.New("该订单已存在且支付参数有变化，请更换订单号")
			}
			return s.buildOrderResponse(&existingOrder, &merchant)
		}
		if existingOrder.Status == model.OrderStatusPaid {
			return nil, errors.New("该订单已完成支付，请勿重复发起支付")
		}
		return nil, errors.New("订单号已存在")
	}

	// 确定使用的通道
	channel := "local"
	if !typePending {
		channel = s.determineChannel(req.Channel, chain)
	}

	// 获取订单过期时间
	expireMinutes := s.getOrderExpireMinutes()
	expiredAt := time.Now().Add(time.Duration(expireMinutes) * time.Minute)

	// 使用新的货币转换服务
	rateService := GetRateService()
	var payAmount, settlementAmount decimal.Decimal
	var payCurrency string

	// 1. 计算结算金额（USD）- 使用买入汇率，计入商户余额
	settlementResult, err := rateService.ConvertToSettlementCurrency(currency, money)
	if err != nil {
		return nil, errors.New("结算金额计算失败: " + err.Error())
	}
	settlementAmount = settlementResult.Amount
	// rate := settlementResult.Rate // 原始货币 -> USD 的买入汇率（已不需要）

	// 2. 计算支付金额（type 未选时仅按 CNY 展示，选 type 后再精算）
	if typePending {
		payCurrency = currency
		if currency == "CNY" {
			payAmount = money.Round(2)
		} else {
			payAmount = settlementAmount
		}
	} else if isFiat {
		payCurrency = "CNY"
		cnyUsdRate, err := rateService.GetRateWithType(RateTypeBuy, "USD", "CNY")
		if err != nil {
			return nil, errors.New("CNY汇率获取失败: " + err.Error())
		}
		payAmount = settlementAmount.Mul(cnyUsdRate).Round(2)
	} else {
		switch chain {
		case "trx":
			payCurrency = "TRX"
		default:
			payCurrency = "USDT"
		}
		buyFloatStr := rateService.GetConfigValue(model.ConfigKeyRateBuyFloat, "0")
		buyFloat, _ := decimal.NewFromString(buyFloatStr)
		if payCurrency == "USDT" {
			if buyFloat.IsZero() {
				payAmount = settlementAmount.Round(6)
			} else {
				payAmount = settlementAmount.Div(decimal.NewFromInt(1).Sub(buyFloat)).Round(6)
			}
		} else if payCurrency == "TRX" {
			trxUsdRate, err := rateService.GetTRXUSDRate()
			if err != nil {
				return nil, errors.New("TRX汇率获取失败: " + err.Error())
			}
			adjustedRate := trxUsdRate
			if !buyFloat.IsZero() {
				adjustedRate = trxUsdRate.Mul(decimal.NewFromInt(1).Sub(buyFloat))
			}
			payAmount = settlementAmount.Div(adjustedRate).Round(6)
		}
	}

	// 计算显示汇率（用于收银台显示）
	// 显示格式: 1 {支付货币} ≈ {产品货币符号}X
	// 例如：1 USDT ≈ $1（USD产品）、1 TRX ≈ €0.84（EUR产品）
	var displayRate decimal.Decimal
	if payCurrency == currency {
		// 支付货币与产品货币相同，汇率为1
		displayRate = decimal.NewFromInt(1)
	} else if !payAmount.IsZero() {
		// 计算：1单位支付货币 = 多少产品货币
		// displayRate = money / payAmount
		displayRate = money.Div(payAmount)
	} else {
		displayRate = decimal.NewFromInt(1)
	}

	// 创建订单
	order := model.Order{
		TradeNo:          util.GenerateTradeNo(),
		OutTradeNo:       req.OutTradeNo,
		MerchantID:       merchant.ID,
		Type:             payType,
		Name:             req.Name,
		Currency:         currency,
		Money:            money,
		PayAmount:        payAmount,
		PayCurrency:      payCurrency,
		USDTAmount:       payAmount,        // 兼容旧字段
		SettlementAmount: settlementAmount, // 结算金额（USD），用于计入商户余额
		Rate:             displayRate,      // 显示汇率（支付货币 -> CNY）
		Chain:            chain,
		Status:           model.OrderStatusPending,
		NotifyURL:        req.NotifyURL,
		ReturnURL:        req.ReturnURL,
		Param:            req.Param,
		ClientIP:         req.ClientIP,
		ExpiredAt:        expiredAt,
		Channel:          channel,
	}

	// 根据通道类型处理（type 待选时不绑定钱包，等收银台选择）
	if channel == "local" && !typePending {
		if isFiat {
			// 法币：优先官方通道，失败再个人码
			if err := s.bindFiatPayment(&order, &merchant, chain, payAmount, settlementAmount); err != nil {
				return nil, err
			}
		} else {
			wallet, useMerchantWallet, err := s.selectWalletByMode(&merchant, chain)
			if err != nil {
				return nil, err
			}
			if chain == "trc20" || chain == "trx" {
				order.ToAddress = wallet.Address
			} else {
				order.ToAddress = strings.ToLower(wallet.Address)
			}
			uniqueAmount := rateService.GenerateUniqueAmount(payAmount, chain)
			order.PayAmount = payAmount
			order.UniqueAmount = uniqueAmount
			order.USDTAmount = uniqueAmount
			order.WalletID = wallet.ID
			if err := s.applyFee(&order, &merchant, settlementAmount, useMerchantWallet); err != nil {
				return nil, err
			}
		}
	}

	if err := model.GetDB().Create(&order).Error; err != nil {
		return nil, errors.New("订单创建失败")
	}

	// 官方通道：落库后拿 trade_no 调上游预下单
	if !typePending && (order.Channel == "alipay_official" || order.Channel == "wechat_official") {
		if err := s.createOfficialPayment(&order); err != nil {
			// 官方失败则尝试回退个人码
			log.Printf("[CreateOrder] official pay failed, fallback personal: %v", err)
			if err2 := s.fallbackPersonalQR(&order, &merchant, chain, payAmount, settlementAmount); err2 != nil {
				model.GetDB().Delete(&order)
				return nil, fmt.Errorf("官方支付失败且个人码不可用: %v / %v", err, err2)
			}
			model.GetDB().Save(&order)
		} else {
			model.GetDB().Save(&order)
		}
	}

	// 发送Telegram通知 - 订单创建
	go GetTelegramService().NotifyOrderCreated(&order)

	// 上游易支付通道
	if channel != "local" && !typePending {
		if err := s.createUpstreamOrder(&order, channel); err != nil {
			return nil, fmt.Errorf("上游通道下单失败: %w", err)
		}
		model.GetDB().Save(&order)
	}

	return s.buildOrderResponse(&order, &merchant)
}

// bindFiatPayment 绑定法币支付（标记官方或个人码，官方实际下单在 create 后）
func (s *OrderService) bindFiatPayment(order *model.Order, merchant *model.Merchant, chain string, payAmount, settlementAmount decimal.Decimal) error {
	// 官方模式且驱动可用：先占位，create 后再填二维码
	if _, err := payment.OfficialDriverForChain(chain); err == nil {
		order.Channel = chain + "_official" // alipay_official / wechat_official
		order.PayAmount = payAmount
		order.UniqueAmount = payAmount
		order.USDTAmount = payAmount
		order.FeeType = model.FeeTypeDeduction
		order.FeeRate = s.getSystemWalletFeeRate()
		order.Fee = settlementAmount.Mul(order.FeeRate)
		return nil
	}
	// 个人收款码
	return s.fallbackPersonalQR(order, merchant, chain, payAmount, settlementAmount)
}

func (s *OrderService) fallbackPersonalQR(order *model.Order, merchant *model.Merchant, chain string, payAmount, settlementAmount decimal.Decimal) error {
	rateService := GetRateService()
	wallet, useMerchantWallet, err := s.selectWalletByMode(merchant, chain)
	if err != nil {
		return err
	}
	order.Channel = "local"
	order.ToAddress = wallet.Address
	order.QRCode = wallet.QRCode
	order.WalletID = wallet.ID
	uniqueAmount := rateService.GenerateUniqueAmount(payAmount, chain)
	order.PayAmount = payAmount
	order.UniqueAmount = uniqueAmount
	order.USDTAmount = uniqueAmount
	return s.applyFee(order, merchant, settlementAmount, useMerchantWallet)
}

func (s *OrderService) applyFee(order *model.Order, merchant *model.Merchant, settlementAmount decimal.Decimal, useMerchantWallet bool) error {
	var feeRate decimal.Decimal
	if useMerchantWallet {
		feeRate = s.getPersonalWalletFeeRate()
		order.FeeType = model.FeeTypeBalance
	} else {
		feeRate = s.getSystemWalletFeeRate()
		order.FeeType = model.FeeTypeDeduction
	}
	order.FeeRate = feeRate
	fee := settlementAmount.Mul(feeRate)
	order.Fee = fee
	if useMerchantWallet && fee.GreaterThan(decimal.Zero) {
		result := model.GetDB().Model(&model.Merchant{}).
			Where("id = ? AND (balance - frozen_balance) >= ?", merchant.ID, fee.InexactFloat64()).
			Update("frozen_balance", gorm.Expr("frozen_balance + ?", fee.InexactFloat64()))
		if result.Error != nil {
			return errors.New("扣除手续费失败")
		}
		if result.RowsAffected == 0 {
			return errors.New("商户余额不足以支付手续费，请先充值")
		}
	}
	return nil
}

// buildOfficialNotifyURL 构造官方回调地址
func (s *OrderService) buildOfficialNotifyURL(channel string) string {
	site := payment.GetConfig(payment.CfgSiteURL, "")
	site = strings.TrimRight(site, "/")
	if site == "" {
		return ""
	}
	// channel: alipay_official -> alipay, wechat_official -> wechat
	path := channel
	switch channel {
	case "alipay_official":
		path = "alipay"
	case "wechat_official":
		path = "wechat"
	case "epay":
		path = "epay"
	}
	return site + "/api/channel/notify/" + path
}

// createOfficialPayment 调用支付宝/微信官方预下单
func (s *OrderService) createOfficialPayment(order *model.Order) error {
	chain := order.Chain
	drv, err := payment.OfficialDriverForChain(chain)
	if err != nil {
		return err
	}
	notifyURL := s.buildOfficialNotifyURL(order.Channel)
	if notifyURL == "" {
		return errors.New("请先在系统设置配置 site_url（公网 HTTPS 地址）用于官方支付回调")
	}

	// 官方下单金额：CNY 两位
	amountCNY := order.PayAmount.StringFixed(2)
	if order.PayCurrency != "CNY" {
		// 回退用订单 money 若本身是 CNY
		if order.Currency == "CNY" {
			amountCNY = order.Money.StringFixed(2)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	result, err := drv.Create(ctx, &payment.CreateRequest{
		TradeNo:    order.TradeNo,
		OutTradeNo: order.OutTradeNo,
		Subject:    order.Name,
		AmountCNY:  amountCNY,
		ClientIP:   order.ClientIP,
		NotifyURL:  notifyURL,
		ReturnURL:  order.ReturnURL,
	})
	if err != nil {
		return err
	}

	order.Channel = result.Channel
	order.ChannelOrderID = result.UpstreamID
	order.ChannelPayURL = result.PayURL
	// 官方码内容放 ToAddress，收银台用 QRCode 库生成；同时 QRCode 字段可存图片路径
	order.ToAddress = result.QRContent
	order.QRCode = "" // 前端按 address/code_url 生成
	return nil
}

// SelectPaymentType 收银台选择支付方式（type 为空时创建的订单）
func (s *OrderService) SelectPaymentType(tradeNo, payType string) (*CreateOrderResponse, error) {
	order, err := s.GetOrder(tradeNo)
	if err != nil {
		return nil, err
	}
	if order.Status != model.OrderStatusPending {
		return nil, errors.New("订单状态不允许选择支付方式")
	}
	if order.Chain != "" && order.Type != "" {
		// 已选过：直接返回
		var merchant model.Merchant
		model.GetDB().First(&merchant, order.MerchantID)
		return s.buildOrderResponse(order, &merchant)
	}

	meta, err := util.ResolvePaymentType(payType)
	if err != nil || meta == nil {
		return nil, errors.New("不支持的支付类型")
	}

	var merchant model.Merchant
	if err := model.GetDB().First(&merchant, order.MerchantID).Error; err != nil {
		return nil, errors.New("商户不存在")
	}

	// 用当前订单金额重新走绑定钱包逻辑：构造请求复用 CreateOrder 中的结算字段
	chain := meta.Chain
	normalized := util.NormalizePaymentType(payType)
	isFiat := meta.IsFiat
	rateService := GetRateService()

	settlementAmount := order.SettlementAmount
	var payAmount decimal.Decimal
	var payCurrency string
	if isFiat {
		payCurrency = "CNY"
		cnyUsdRate, err := rateService.GetRateWithType(RateTypeBuy, "USD", "CNY")
		if err != nil {
			return nil, errors.New("CNY汇率获取失败")
		}
		payAmount = settlementAmount.Mul(cnyUsdRate).Round(2)
	} else if chain == "trx" {
		payCurrency = "TRX"
		trxUsdRate, err := rateService.GetTRXUSDRate()
		if err != nil {
			return nil, errors.New("TRX汇率获取失败")
		}
		payAmount = settlementAmount.Div(trxUsdRate).Round(6)
	} else {
		payCurrency = "USDT"
		payAmount = settlementAmount.Round(6)
	}

	order.Type = normalized
	order.Chain = chain
	order.PayCurrency = payCurrency
	order.PayAmount = payAmount

	if isFiat {
		if err := s.bindFiatPayment(order, &merchant, chain, payAmount, order.SettlementAmount); err != nil {
			return nil, err
		}
	} else {
		wallet, useMerchantWallet, err := s.selectWalletByMode(&merchant, chain)
		if err != nil {
			return nil, err
		}
		if chain == "trc20" || chain == "trx" {
			order.ToAddress = wallet.Address
		} else {
			order.ToAddress = strings.ToLower(wallet.Address)
		}
		uniqueAmount := rateService.GenerateUniqueAmount(payAmount, chain)
		order.UniqueAmount = uniqueAmount
		order.USDTAmount = uniqueAmount
		order.WalletID = wallet.ID
		if err := s.applyFee(order, &merchant, order.SettlementAmount, useMerchantWallet); err != nil {
			return nil, err
		}
	}

	if err := model.GetDB().Save(order).Error; err != nil {
		return nil, errors.New("更新支付方式失败")
	}

	if order.Channel == "alipay_official" || order.Channel == "wechat_official" {
		if err := s.createOfficialPayment(order); err != nil {
			log.Printf("[SelectPaymentType] official failed, fallback: %v", err)
			if err2 := s.fallbackPersonalQR(order, &merchant, chain, payAmount, order.SettlementAmount); err2 != nil {
				return nil, fmt.Errorf("官方支付失败且个人码不可用: %v / %v", err, err2)
			}
		}
		model.GetDB().Save(order)
	}

	s.InvalidateOrderCache(tradeNo)
	model.GetDB().First(order, order.ID)
	return s.buildOrderResponse(order, &merchant)
}

// RefundOrder 易支付兼容退款（余额冲正，非原路退）
func (s *OrderService) RefundOrder(merchantPID, tradeNo, outTradeNo, moneyStr string) (map[string]interface{}, error) {
	var merchant model.Merchant
	if err := model.GetDB().Where("p_id = ?", merchantPID).First(&merchant).Error; err != nil {
		return nil, errors.New("商户不存在")
	}

	var order model.Order
	var err error
	if tradeNo != "" {
		err = model.GetDB().Where("merchant_id = ? AND trade_no = ?", merchant.ID, tradeNo).First(&order).Error
	} else if outTradeNo != "" {
		err = model.GetDB().Where("merchant_id = ? AND out_trade_no = ?", merchant.ID, outTradeNo).First(&order).Error
	} else {
		return nil, errors.New("订单号不能为空")
	}
	if err != nil {
		return nil, errors.New("当前订单不存在")
	}
	if order.Status != model.OrderStatusPaid {
		return nil, errors.New("仅已支付订单可退款")
	}

	refundMoney, err := decimal.NewFromString(moneyStr)
	if err != nil || refundMoney.LessThanOrEqual(decimal.Zero) {
		return nil, errors.New("金额输入错误")
	}
	// 订单原币金额
	remain := order.Money.Sub(order.RefundMoney)
	if refundMoney.GreaterThan(remain) {
		return nil, errors.New("退款金额超过可退余额")
	}

	// 按比例从商户 USD 余额扣回
	ratio := refundMoney.Div(order.Money)
	deductUSD := order.SettlementAmount.Mul(ratio)
	if merchant.Balance < deductUSD.InexactFloat64() {
		return nil, errors.New("商户余额不足，无法退款")
	}

	newRefund := order.RefundMoney.Add(refundMoney)
	newStatus := order.Status
	if newRefund.GreaterThanOrEqual(order.Money) {
		newStatus = model.OrderStatusRefunded
	}

	tx := model.GetDB().Begin()
	if err := tx.Model(&merchant).Update("balance", gorm.Expr("balance - ?", deductUSD.InexactFloat64())).Error; err != nil {
		tx.Rollback()
		return nil, errors.New("扣减余额失败")
	}
	if err := tx.Model(&order).Updates(map[string]interface{}{
		"refund_money": newRefund,
		"status":       newStatus,
	}).Error; err != nil {
		tx.Rollback()
		return nil, errors.New("更新订单失败")
	}
	tx.Commit()

	refundNo := time.Now().Format("20060102150405") + util.GenerateRandomHex(3)
	return map[string]interface{}{
		"code":         0,
		"msg":          fmt.Sprintf("退款成功！退款金额¥%s", refundMoney.StringFixed(2)),
		"refund_no":    refundNo,
		"trade_no":     order.TradeNo,
		"out_trade_no": order.OutTradeNo,
		"money":        refundMoney.StringFixed(2),
	}, nil
}

// determineChannel 确定使用的支付通道
func (s *OrderService) determineChannel(requestedChannel string, chain string) string {
	// 如果明确指定了通道，尝试使用该通道
	if requestedChannel != "" && requestedChannel != "local" {
		channelService := GetChannelService()
		if _, err := channelService.GetChannelConfig(ChannelType(requestedChannel)); err == nil {
			return requestedChannel
		}
	}

	// 检查是否有默认的上游通道配置
	var config model.SystemConfig
	if err := model.GetDB().Where("`key` = ?", "default_channel").First(&config).Error; err == nil {
		if config.Value != "" && config.Value != "local" {
			channelService := GetChannelService()
			if _, err := channelService.GetChannelConfig(ChannelType(config.Value)); err == nil {
				return config.Value
			}
		}
	}

	return "local"
}

// createUpstreamOrder 在上游通道创建订单
func (s *OrderService) createUpstreamOrder(order *model.Order, channel string) error {
	channelService := GetChannelService()
	cfg, err := channelService.GetChannelConfig(ChannelType(channel))
	if err != nil {
		return err
	}

	// 构建本系统的回调地址
	var notifyURL string
	var siteConfig model.SystemConfig
	if err := model.GetDB().Where("`key` = ?", "site_url").First(&siteConfig).Error; err == nil {
		notifyURL = siteConfig.Value + "/channel/notify/" + channel
	} else {
		notifyURL = "/channel/notify/" + channel
	}

	var result *ChannelOrderResult

	switch ChannelType(channel) {
	case ChannelTypeEpay:
		result, err = channelService.CreateEpayOrder(cfg, order, notifyURL)
	default:
		return fmt.Errorf("unsupported channel type: %s", channel)
	}

	if err != nil {
		return err
	}

	if !result.Success {
		return fmt.Errorf("upstream error: %s", result.Error)
	}

	// 更新订单
	order.ChannelOrderID = result.OrderID
	order.ChannelPayURL = result.PayURL
	order.PayAmount = result.Amount      // 设置展示金额
	order.UniqueAmount = result.Amount   // 设置标识金额（上游通道不需要偏移）
	order.USDTAmount = result.Amount     // 兼容旧字段
	if !result.ExpireTime.IsZero() {
		order.ExpiredAt = result.ExpireTime
	}
	// 注意：SettlementAmount 已经在订单创建时设置（第202行），这里无需修改

	return model.GetDB().Save(order).Error
}

// buildOrderResponse 构建订单响应
func (s *OrderService) buildOrderResponse(order *model.Order, merchant *model.Merchant) (*CreateOrderResponse, error) {
	// 判断是否为法币收款方式
	isFiat := util.IsFiatChain(order.Chain)
	isOfficial := order.Channel == "alipay_official" || order.Channel == "wechat_official"

	// 生成二维码内容
	var qrcode string
	if isOfficial {
		// 官方：code_url / qr_code 存在 ToAddress
		qrcode = order.ToAddress
	} else if isFiat {
		// 个人码：图片路径
		qrcode = order.QRCode
		if qrcode == "" {
			qrcode = order.ToAddress
		}
	} else {
		// USDT使用收款地址
		qrcode = order.ToAddress
	}

	resp := &CreateOrderResponse{
		Code:         1,
		Msg:          "success",
		TradeNo:      order.TradeNo,
		OutTradeNo:   order.OutTradeNo,
		Type:         util.ToEpayType(order.Type),
		Currency:     order.Currency,
		Money:        order.Money.StringFixed(2),
		PayCurrency:  order.PayCurrency,
		PayAmount:    order.PayAmount.String(),
		UniqueAmount: order.UniqueAmount.String(),
		USDTAmount:   order.UniqueAmount.String(),
		Rate:         order.Rate.String(),
		Address:      order.ToAddress,
		Chain:        order.Chain,
		QRCode:       qrcode,
		ExpiredAt:    order.ExpiredAt.Format("2006-01-02 15:04:05"),
		Channel:      order.Channel,
		PayURL:       "/cashier/" + order.TradeNo,
	}

	if order.Channel != "local" && order.ChannelPayURL != "" {
		resp.ChannelPayURL = order.ChannelPayURL
		resp.PayURL = order.ChannelPayURL
	}

	return resp, nil
}

// GetOrder 获取订单
func (s *OrderService) GetOrder(tradeNo string) (*model.Order, error) {
	var order model.Order
	if err := model.GetDB().Preload("Merchant").Where("trade_no = ?", tradeNo).First(&order).Error; err != nil {
		return nil, errors.New("订单不存在")
	}
	return &order, nil
}

// GetOrderByOutTradeNo 根据商户订单号获取订单
// 如果merchantPID为空，则只根据out_trade_no查询
func (s *OrderService) GetOrderByOutTradeNo(merchantPID, outTradeNo string) (*model.Order, error) {
	var order model.Order

	if merchantPID == "" {
		// 不指定商户，直接按商户订单号查询
		if err := model.GetDB().Preload("Merchant").Where("out_trade_no = ?", outTradeNo).First(&order).Error; err != nil {
			return nil, errors.New("订单不存在")
		}
	} else {
		// 指定商户，先查商户再查订单
		var merchant model.Merchant
		if err := model.GetDB().Where("p_id = ?", merchantPID).First(&merchant).Error; err != nil {
			return nil, errors.New("商户不存在")
		}

		if err := model.GetDB().Where("merchant_id = ? AND out_trade_no = ?", merchant.ID, outTradeNo).First(&order).Error; err != nil {
			return nil, errors.New("订单不存在")
		}
		order.Merchant = &merchant
	}

	return &order, nil
}

// GetOrderStatus 获取订单状态
func (s *OrderService) GetOrderStatus(tradeNo string) (model.OrderStatus, error) {
	var order model.Order
	if err := model.GetDB().Select("status").Where("trade_no = ?", tradeNo).First(&order).Error; err != nil {
		return 0, errors.New("订单不存在")
	}
	return order.Status, nil
}

// QueryOrders 查询订单列表
func (s *OrderService) QueryOrders(query *model.OrderQuery) ([]model.Order, int64, error) {
	db := model.GetDB().Model(&model.Order{})

	if query.MerchantID > 0 {
		db = db.Where("merchant_id = ?", query.MerchantID)
	}
	if query.TradeNo != "" {
		db = db.Where("trade_no = ?", query.TradeNo)
	}
	if query.OutTradeNo != "" {
		db = db.Where("out_trade_no = ?", query.OutTradeNo)
	}
	if query.Status != nil {
		db = db.Where("status = ?", *query.Status)
	}
	if query.Type != "" {
		db = db.Where("type = ?", query.Type)
	}
	if query.StartTime != nil {
		db = db.Where("created_at >= ?", query.StartTime)
	}
	if query.EndTime != nil {
		db = db.Where("created_at <= ?", query.EndTime)
	}

	var total int64
	db.Count(&total)

	if query.Page <= 0 {
		query.Page = 1
	}
	if query.PageSize <= 0 {
		query.PageSize = 20
	}

	var orders []model.Order
	offset := (query.Page - 1) * query.PageSize
	if err := db.Preload("Merchant").Order("created_at DESC").Offset(offset).Limit(query.PageSize).Find(&orders).Error; err != nil {
		return nil, 0, err
	}

	return orders, total, nil
}

// QueryOrdersWithMerchant 查询订单列表（带商户信息，用于管理后台）
func (s *OrderService) QueryOrdersWithMerchant(query *model.OrderQuery) ([]model.Order, int64, error) {
	// 直接调用 QueryOrders，因为它已经使用 Preload("Merchant")
	return s.QueryOrders(query)
}

// GetOrderStats 获取订单统计 (金额统一使用 USD，USDT ≈ USD)
func (s *OrderService) GetOrderStats(merchantID uint) (*model.OrderStats, error) {
	stats := &model.OrderStats{}
	db := model.GetDB().Model(&model.Order{})

	if merchantID > 0 {
		db = db.Where("merchant_id = ?", merchantID)
	}

	// 总订单数
	db.Count(&stats.TotalOrders)

	// 总金额 (使用 settlement_amount 统一 USD 结算)
	var totalUSD float64
	dbPaid := model.GetDB().Model(&model.Order{})
	if merchantID > 0 {
		dbPaid = dbPaid.Where("merchant_id = ?", merchantID)
	}
	dbPaid.Where("status = ?", model.OrderStatusPaid).Select("COALESCE(SUM(settlement_amount), 0)").Scan(&totalUSD)
	stats.TotalUSD = decimal.NewFromFloat(totalUSD)

	// 各状态订单数
	dbPending := model.GetDB().Model(&model.Order{})
	if merchantID > 0 {
		dbPending = dbPending.Where("merchant_id = ?", merchantID)
	}
	dbPending.Where("status = ?", model.OrderStatusPending).Count(&stats.PendingOrders)

	dbPaidCount := model.GetDB().Model(&model.Order{})
	if merchantID > 0 {
		dbPaidCount = dbPaidCount.Where("merchant_id = ?", merchantID)
	}
	dbPaidCount.Where("status = ?", model.OrderStatusPaid).Count(&stats.PaidOrders)

	dbExpired := model.GetDB().Model(&model.Order{})
	if merchantID > 0 {
		dbExpired = dbExpired.Where("merchant_id = ?", merchantID)
	}
	dbExpired.Where("status = ?", model.OrderStatusExpired).Count(&stats.ExpiredOrders)

	// 今日统计
	today := time.Now().Format("2006-01-02")
	todayDB := model.GetDB().Model(&model.Order{}).Where("DATE(created_at) = ?", today)
	if merchantID > 0 {
		todayDB = todayDB.Where("merchant_id = ?", merchantID)
	}

	todayDB.Count(&stats.TodayOrders)

	var todayUSD float64
	todayDB.Where("status = ?", model.OrderStatusPaid).Select("COALESCE(SUM(settlement_amount), 0)").Scan(&todayUSD)
	stats.TodayUSD = decimal.NewFromFloat(todayUSD)

	// 可用支付链路数量
	blockchainService := GetBlockchainService()
	listenerStatus := blockchainService.GetListenerStatus()
	availableChannels := 0
	for _, infoInterface := range listenerStatus {
		if info, ok := infoInterface.(map[string]interface{}); ok {
			running, _ := info["running"].(bool)
			walletCount, _ := info["wallet_count"].(int64)
			if running && walletCount > 0 {
				availableChannels++
			}
		}
	}
	stats.AvailableChannels = availableChannels

	return stats, nil
}

// ExpireOrders 过期订单处理
func (s *OrderService) ExpireOrders() {
	// 查找所有即将过期的订单
	var orders []model.Order
	if err := model.GetDB().Where("status = ? AND expired_at < ?", model.OrderStatusPending, time.Now()).Find(&orders).Error; err != nil {
		return
	}

	for _, order := range orders {
		// 更新订单状态
		model.GetDB().Model(&order).Update("status", model.OrderStatusExpired)

		// 退还预扣的手续费 (仅商户钱包模式)
		if order.FeeType == model.FeeTypeBalance {
			fee, _ := order.Fee.Float64()
			if err := GetWithdrawService().RefundPreChargedFee(order.MerchantID, fee); err != nil {
				fmt.Printf("Failed to refund fee for expired order %s: %v\n", order.TradeNo, err)
			}
		}

		// 发送Telegram通知 - 订单过期
		go GetTelegramService().NotifyOrderExpired(&order)
	}

	if len(orders) > 0 {
		fmt.Printf("Expired %d orders\n", len(orders))
	}
}

// StartExpireWorker 启动订单过期处理工作协程
func (s *OrderService) StartExpireWorker() {
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()

		for range ticker.C {
			s.ExpireOrders()
		}
	}()
}

// getOrderExpireMinutes 获取订单过期时间(分钟)
func (s *OrderService) getOrderExpireMinutes() int {
	var config model.SystemConfig
	if err := model.GetDB().Where("`key` = ?", model.ConfigKeyOrderExpire).First(&config).Error; err != nil {
		return 30
	}
	minutes, err := strconv.Atoi(config.Value)
	if err != nil {
		return 30
	}
	return minutes
}

// selectWalletByMode 根据商户钱包模式选择钱包
// WalletMode: 1=仅系统钱包, 2=仅个人钱包, 3=混合模式(优先个人)
// 返回: 钱包, 是否为商户钱包, 错误
// 使用轮询方式选择钱包，优先选择最久未使用的钱包
func (s *OrderService) selectWalletByMode(merchant *model.Merchant, chain string) (model.Wallet, bool, error) {
	var wallet model.Wallet
	var useMerchantWallet bool

	// 轮询排序：按最后使用时间升序（NULL值优先，即从未使用的钱包优先）
	roundRobinOrder := "COALESCE(last_used_at, '1970-01-01') ASC"

	switch merchant.WalletMode {
	case 1: // 仅系统钱包
		if err := model.GetDB().Where("chain = ? AND status = 1 AND (merchant_id IS NULL OR merchant_id = 0)", chain).Order(roundRobinOrder).First(&wallet).Error; err != nil {
			return wallet, false, fmt.Errorf("暂无可用的系统收款地址")
		}
		useMerchantWallet = false

	case 2: // 仅个人钱包
		if err := model.GetDB().Where("chain = ? AND status = 1 AND merchant_id = ?", chain, merchant.ID).Order(roundRobinOrder).First(&wallet).Error; err != nil {
			return wallet, false, fmt.Errorf("暂无可用的个人收款地址，请先添加收款地址")
		}
		useMerchantWallet = true

	default: // 3=混合模式(优先个人)
		// 优先使用商户自己的钱包
		if err := model.GetDB().Where("chain = ? AND status = 1 AND merchant_id = ?", chain, merchant.ID).Order(roundRobinOrder).First(&wallet).Error; err != nil {
			// 没有商户钱包，使用系统钱包
			if err := model.GetDB().Where("chain = ? AND status = 1 AND (merchant_id IS NULL OR merchant_id = 0)", chain).Order(roundRobinOrder).First(&wallet).Error; err != nil {
				return wallet, false, fmt.Errorf("暂无可用的收款地址")
			}
			useMerchantWallet = false
		} else {
			useMerchantWallet = true
		}
	}

	// 更新钱包最后使用时间
	model.GetDB().Model(&wallet).Update("last_used_at", time.Now())

	return wallet, useMerchantWallet, nil
}

// getSystemWalletFeeRate 获取系统收款码手续费率
func (s *OrderService) getSystemWalletFeeRate() decimal.Decimal {
	var config model.SystemConfig
	if err := model.GetDB().Where("`key` = ?", model.ConfigKeySystemWalletFeeRate).First(&config).Error; err != nil {
		return decimal.NewFromFloat(0.02) // 默认2%
	}
	rate, err := decimal.NewFromString(config.Value)
	if err != nil {
		return decimal.NewFromFloat(0.02)
	}
	return rate
}

// getPersonalWalletFeeRate 获取个人收款码手续费率
func (s *OrderService) getPersonalWalletFeeRate() decimal.Decimal {
	var config model.SystemConfig
	if err := model.GetDB().Where("`key` = ?", model.ConfigKeyPersonalWalletFeeRate).First(&config).Error; err != nil {
		return decimal.NewFromFloat(0.01) // 默认1%
	}
	rate, err := decimal.NewFromString(config.Value)
	if err != nil {
		return decimal.NewFromFloat(0.01)
	}
	return rate
}

// CancelOrder 取消订单
func (s *OrderService) CancelOrder(tradeNo string) error {
	var order model.Order
	if err := model.GetDB().Where("trade_no = ? AND status = ?", tradeNo, model.OrderStatusPending).First(&order).Error; err != nil {
		return errors.New("订单不存在或无法取消")
	}

	// 更新订单状态
	if err := model.GetDB().Model(&order).Update("status", model.OrderStatusCancelled).Error; err != nil {
		return err
	}

	// 退还预扣的手续费 (仅商户钱包模式)
	if order.FeeType == model.FeeTypeBalance {
		fee, _ := order.Fee.Float64()
		if err := GetWithdrawService().RefundPreChargedFee(order.MerchantID, fee); err != nil {
			return fmt.Errorf("退还手续费失败: %v", err)
		}
	}

	return nil
}

// MarkOrderPaid 手动标记订单已支付 (仅管理员)
func (s *OrderService) MarkOrderPaid(tradeNo string, txHash string, amount decimal.Decimal) error {
	var order model.Order
	if err := model.GetDB().Where("trade_no = ?", tradeNo).First(&order).Error; err != nil {
		return errors.New("订单不存在")
	}

	if order.Status != model.OrderStatusPending && order.Status != model.OrderStatusExpired {
		return errors.New("订单状态不正确，只能确认待支付或已过期订单")
	}

	now := time.Now()
	if txHash == "" {
		txHash = "MANUAL_ADMIN_" + now.Format("20060102150405")
	}
	if amount.IsZero() {
		amount = order.UniqueAmount
	}

	// 乐观锁：仅从待支付/已过期变为已支付，防止重复入账
	updates := map[string]interface{}{
		"status":        model.OrderStatusPaid,
		"tx_hash":       txHash,
		"actual_amount": amount,
		"paid_at":       &now,
	}

	result := model.GetDB().Model(&order).
		Where("status IN ?", []model.OrderStatus{model.OrderStatusPending, model.OrderStatusExpired}).
		Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New("订单状态已变更，请刷新后重试")
	}

	// 使缓存失效
	s.InvalidateOrderCache(tradeNo)

	// 重新加载订单数据
	model.GetDB().First(&order, order.ID)

	// 入账（与链上匹配路径一致）
	if err := s.CreditMerchantBalance(&order); err != nil {
		// 入账失败记日志，但不回滚已支付状态（需人工核对）
		// 调用方仍可看到 success；日志供运维排查
		return fmt.Errorf("订单已标记支付，但入账失败: %w", err)
	}

	// 触发回调
	go GetNotifyService().NotifyOrder(order.ID)

	// 触发 Telegram 通知
	go GetTelegramService().NotifyOrderPaid(&order)

	return nil
}

// CreditMerchantBalance 订单支付成功后增加商户余额（幂等依赖调用方先用乐观锁改状态）
func (s *OrderService) CreditMerchantBalance(order *model.Order) error {
	if order == nil {
		return errors.New("order is nil")
	}
	settlementAmount, _ := order.SettlementAmount.Float64()
	fee, _ := order.Fee.Float64()
	if err := GetWithdrawService().AddMerchantBalance(order.MerchantID, settlementAmount, fee, order.FeeType); err != nil {
		return err
	}
	return nil
}

// CheckOrderPaid 检查订单是否已支付 (用于轮询)
func (s *OrderService) CheckOrderPaid(tradeNo string) (bool, *model.Order, error) {
	// 检查缓存
	if cached, ok := s.cache.Load(tradeNo); ok {
		item := cached.(orderCacheItem)
		// 如果缓存未过期且订单已支付或已过期，直接返回缓存结果
		// 已支付和已过期的订单状态不会再变化，可以长期缓存
		if time.Since(item.timestamp) < s.cacheTTL || item.order.Status == model.OrderStatusPaid || item.order.Status == model.OrderStatusExpired {
			return item.paid, item.order, nil
		}
	}

	// 从数据库查询
	var order model.Order
	if err := model.GetDB().Where("trade_no = ?", tradeNo).First(&order).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil, errors.New("订单不存在")
		}
		return false, nil, err
	}

	paid := order.Status == model.OrderStatusPaid

	// 更新缓存
	s.cache.Store(tradeNo, orderCacheItem{
		order:     &order,
		paid:      paid,
		timestamp: time.Now(),
	})

	return paid, &order, nil
}

// InvalidateOrderCache 使订单缓存失效（在订单状态更新时调用）
func (s *OrderService) InvalidateOrderCache(tradeNo string) {
	s.cache.Delete(tradeNo)
}
