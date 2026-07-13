package handler

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/HenZenKuriRIP/k2pay/internal/model"
	"github.com/HenZenKuriRIP/k2pay/internal/payment"
	"github.com/HenZenKuriRIP/k2pay/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

// OfficialPayHandler 官方支付宝/微信回调与配置
type OfficialPayHandler struct{}

func NewOfficialPayHandler() *OfficialPayHandler {
	return &OfficialPayHandler{}
}

// AlipayNotify 支付宝异步通知
// POST /channel/notify/alipay_official
func (h *OfficialPayHandler) AlipayNotify(c *gin.Context) {
	if err := c.Request.ParseForm(); err != nil {
		c.String(http.StatusOK, "fail")
		return
	}
	params := map[string]string{}
	for k, vs := range c.Request.PostForm {
		if len(vs) > 0 {
			params[k] = vs[0]
		}
	}
	for k, vs := range c.Request.URL.Query() {
		if _, ok := params[k]; !ok && len(vs) > 0 {
			params[k] = vs[0]
		}
	}

	if err := payment.VerifyAlipayNotify(params); err != nil {
		log.Printf("[AlipayNotify] sign verify failed: %v", err)
		c.String(http.StatusOK, "fail")
		return
	}

	tradeStatus := params["trade_status"]
	if tradeStatus != "TRADE_SUCCESS" && tradeStatus != "TRADE_FINISHED" {
		c.String(http.StatusOK, "success")
		return
	}

	tradeNo := params["out_trade_no"]
	upstreamID := params["trade_no"]
	buyer := params["buyer_id"]
	if buyer == "" {
		buyer = params["buyer_logon_id"]
	}
	amount, _ := decimal.NewFromString(params["total_amount"])

	if err := markOfficialPaid(tradeNo, upstreamID, buyer, amount); err != nil {
		log.Printf("[AlipayNotify] mark paid failed: %v", err)
		c.String(http.StatusOK, "fail")
		return
	}
	c.String(http.StatusOK, "success")
}

// WechatNotify 微信 APIv3 异步通知
// POST /channel/notify/wechat_official
func (h *OfficialPayHandler) WechatNotify(c *gin.Context) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "FAIL", "message": "read body"})
		return
	}

	var envelope struct {
		EventType string `json:"event_type"`
		Resource  struct {
			Ciphertext     string `json:"ciphertext"`
			AssociatedData string `json:"associated_data"`
			Nonce          string `json:"nonce"`
		} `json:"resource"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "FAIL", "message": "parse json"})
		return
	}

	if envelope.EventType != "" && envelope.EventType != "TRANSACTION.SUCCESS" {
		c.JSON(http.StatusOK, gin.H{"code": "SUCCESS", "message": "OK"})
		return
	}

	res, err := payment.DecryptWechatNotify(
		envelope.Resource.AssociatedData,
		envelope.Resource.Nonce,
		envelope.Resource.Ciphertext,
	)
	if err != nil {
		log.Printf("[WechatNotify] decrypt failed: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"code": "FAIL", "message": "decrypt failed"})
		return
	}
	if res.TradeState != "SUCCESS" {
		c.JSON(http.StatusOK, gin.H{"code": "SUCCESS", "message": "OK"})
		return
	}

	amount := decimal.NewFromInt(int64(res.Amount.Total)).Div(decimal.NewFromInt(100))
	if err := markOfficialPaid(res.OutTradeNo, res.TransactionID, "", amount); err != nil {
		log.Printf("[WechatNotify] mark paid failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": "FAIL", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": "SUCCESS", "message": "成功"})
}

func markOfficialPaid(tradeNo, upstreamID, buyer string, amount decimal.Decimal) error {
	var order model.Order
	if err := model.GetDB().Where("trade_no = ?", tradeNo).First(&order).Error; err != nil {
		return err
	}
	if order.Status == model.OrderStatusPaid || order.Status == model.OrderStatusRefunded {
		return nil
	}
	if order.Status != model.OrderStatusPending {
		return nil
	}

	now := time.Now()
	result := model.GetDB().Model(&order).
		Where("status = ?", model.OrderStatusPending).
		Updates(map[string]interface{}{
			"status":           model.OrderStatusPaid,
			"actual_amount":    amount,
			"api_trade_no":     upstreamID,
			"channel_order_id": upstreamID,
			"tx_hash":          upstreamID,
			"buyer":            buyer,
			"paid_at":          &now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return nil
	}

	model.GetDB().First(&order, order.ID)
	service.GetOrderService().InvalidateOrderCache(tradeNo)
	if err := service.GetOrderService().CreditMerchantBalance(&order); err != nil {
		log.Printf("[OfficialPay] credit failed order=%s: %v", tradeNo, err)
	}
	go service.GetNotifyService().NotifyOrder(order.ID)
	go service.GetTelegramService().NotifyOrderPaid(&order)
	return nil
}

// GetOfficialPayConfig 管理端：获取官方支付配置（密钥脱敏）
func (h *OfficialPayHandler) GetOfficialPayConfig(c *gin.Context) {
	mask := func(s string) string {
		s = strings.TrimSpace(s)
		if s == "" {
			return ""
		}
		if len(s) <= 8 {
			return "****"
		}
		return s[:4] + "****" + s[len(s)-4:]
	}
	c.JSON(http.StatusOK, gin.H{
		"code": 1,
		"data": gin.H{
			"alipay_mode":        payment.GetConfig(payment.CfgAlipayMode, payment.ModePersonal),
			"alipay_app_id":      payment.GetConfig(payment.CfgAlipayAppID, ""),
			"alipay_private_key": mask(payment.GetConfig(payment.CfgAlipayPrivateKey, "")),
			"alipay_public_key":  mask(payment.GetConfig(payment.CfgAlipayPublicKey, "")),
			"alipay_sandbox":     payment.GetConfig(payment.CfgAlipaySandbox, "0"),
			"alipay_configured":  payment.GetConfig(payment.CfgAlipayPrivateKey, "") != "",
			"wechat_mode":        payment.GetConfig(payment.CfgWechatMode, payment.ModePersonal),
			"wechat_app_id":      payment.GetConfig(payment.CfgWechatAppID, ""),
			"wechat_mch_id":      payment.GetConfig(payment.CfgWechatMchID, ""),
			"wechat_api_v3_key":  mask(payment.GetConfig(payment.CfgWechatAPIv3Key, "")),
			"wechat_serial_no":   payment.GetConfig(payment.CfgWechatSerialNo, ""),
			"wechat_private_key": mask(payment.GetConfig(payment.CfgWechatPrivateKey, "")),
			"wechat_configured":  payment.GetConfig(payment.CfgWechatPrivateKey, "") != "",
			"site_url":           payment.GetConfig(payment.CfgSiteURL, ""),
		},
	})
}

// UpdateOfficialPayConfig 管理端：更新官方支付配置
func (h *OfficialPayHandler) UpdateOfficialPayConfig(c *gin.Context) {
	var req map[string]string
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{"code": -1, "msg": "参数错误"})
		return
	}

	setIf := func(key, val, desc string) {
		val = strings.TrimSpace(val)
		if val == "" || strings.Contains(val, "****") {
			return
		}
		_ = payment.SetConfig(key, val, desc)
	}

	if v, ok := req["alipay_mode"]; ok && (v == payment.ModePersonal || v == payment.ModeOfficial) {
		_ = payment.SetConfig(payment.CfgAlipayMode, v, "支付宝模式")
	}
	setIf(payment.CfgAlipayAppID, req["alipay_app_id"], "支付宝AppID")
	setIf(payment.CfgAlipayPrivateKey, req["alipay_private_key"], "支付宝应用私钥")
	setIf(payment.CfgAlipayPublicKey, req["alipay_public_key"], "支付宝公钥")
	if v, ok := req["alipay_sandbox"]; ok {
		_ = payment.SetConfig(payment.CfgAlipaySandbox, v, "支付宝沙箱")
	}

	if v, ok := req["wechat_mode"]; ok && (v == payment.ModePersonal || v == payment.ModeOfficial) {
		_ = payment.SetConfig(payment.CfgWechatMode, v, "微信模式")
	}
	setIf(payment.CfgWechatAppID, req["wechat_app_id"], "微信AppID")
	setIf(payment.CfgWechatMchID, req["wechat_mch_id"], "微信商户号")
	setIf(payment.CfgWechatAPIv3Key, req["wechat_api_v3_key"], "微信APIv3密钥")
	setIf(payment.CfgWechatSerialNo, req["wechat_serial_no"], "微信证书序列号")
	setIf(payment.CfgWechatPrivateKey, req["wechat_private_key"], "微信API私钥")
	setIf(payment.CfgSiteURL, req["site_url"], "站点公网URL")

	c.JSON(http.StatusOK, gin.H{"code": 1, "msg": "保存成功"})
}
