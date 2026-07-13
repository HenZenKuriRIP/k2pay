package handler

import (
	"encoding/json"
	"fmt"
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

	// 金额与订单校验（允许 0.01 误差）
	var order model.Order
	if err := model.GetDB().Where("trade_no = ?", tradeNo).First(&order).Error; err == nil {
		expect := order.PayAmount
		if expect.IsZero() {
			expect = order.UniqueAmount
		}
		if !expect.IsZero() {
			diff := amount.Sub(expect).Abs()
			if diff.GreaterThan(decimal.NewFromFloat(0.01)) {
				log.Printf("[AlipayNotify] amount mismatch trade=%s got=%s expect=%s", tradeNo, amount.String(), expect.String())
				c.String(http.StatusOK, "fail")
				return
			}
		}
		// 仅接受官方支付宝渠道订单
		if order.Channel != "" && order.Channel != "alipay_official" && order.Chain != "alipay" {
			log.Printf("[AlipayNotify] unexpected channel=%s trade=%s", order.Channel, tradeNo)
		}
	}

	if err := markOfficialPaid(tradeNo, upstreamID, buyer, amount); err != nil {
		log.Printf("[AlipayNotify] mark paid failed: %v", err)
		c.String(http.StatusOK, "fail")
		return
	}
	c.String(http.StatusOK, "success")
}

// WechatNotify 微信 APIv3 异步通知
// POST /api/channel/notify/wechat
func (h *OfficialPayHandler) WechatNotify(c *gin.Context) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "FAIL", "message": "read body"})
		return
	}

	var envelope struct {
		ID           string `json:"id"`
		CreateTime   string `json:"create_time"`
		EventType    string `json:"event_type"`
		ResourceType string `json:"resource_type"`
		Summary      string `json:"summary"`
		Resource     struct {
			Algorithm      string `json:"algorithm"`
			Ciphertext     string `json:"ciphertext"`
			AssociatedData string `json:"associated_data"`
			Nonce          string `json:"nonce"`
			OriginalType   string `json:"original_type"`
		} `json:"resource"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "FAIL", "message": "parse json"})
		return
	}

	// 非成功事件：确认收到即可
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

	// 金额与订单校验（允许 0.01 元误差）
	var order model.Order
	if err := model.GetDB().Where("trade_no = ?", res.OutTradeNo).First(&order).Error; err == nil {
		expect := order.PayAmount
		if expect.IsZero() {
			expect = order.UniqueAmount
		}
		if !expect.IsZero() {
			diff := amount.Sub(expect).Abs()
			if diff.GreaterThan(decimal.NewFromFloat(0.01)) {
				log.Printf("[WechatNotify] amount mismatch trade=%s got=%s expect=%s", res.OutTradeNo, amount.String(), expect.String())
				c.JSON(http.StatusBadRequest, gin.H{"code": "FAIL", "message": "amount mismatch"})
				return
			}
		}
		if order.Channel != "" && order.Channel != "wechat_official" && order.Chain != "wechat" {
			log.Printf("[WechatNotify] unexpected channel=%s trade=%s", order.Channel, res.OutTradeNo)
		}
		// 商户号一致性（可选校验）
		cfgMch := payment.GetConfig(payment.CfgWechatMchID, "")
		if cfgMch != "" && res.MchID != "" && res.MchID != cfgMch {
			log.Printf("[WechatNotify] mchid mismatch got=%s expect=%s", res.MchID, cfgMch)
			c.JSON(http.StatusBadRequest, gin.H{"code": "FAIL", "message": "mchid mismatch"})
			return
		}
	}

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
			"wechat_configured":  payment.WechatConfigComplete(),
			"alipay_ready": payment.GetConfig(payment.CfgAlipayMode, payment.ModePersonal) == payment.ModeOfficial &&
				payment.GetConfig(payment.CfgAlipayAppID, "") != "" &&
				payment.GetConfig(payment.CfgAlipayPrivateKey, "") != "",
			"wechat_ready": payment.GetConfig(payment.CfgWechatMode, payment.ModePersonal) == payment.ModeOfficial &&
				payment.WechatConfigComplete(),
			"site_url": payment.GetConfig(payment.CfgSiteURL, ""),
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

// TestAlipayConfig 测试官方支付宝配置（查询一笔不存在的订单，验证 AppID/密钥是否可用）
func (h *OfficialPayHandler) TestAlipayConfig(c *gin.Context) {
	mode := payment.GetConfig(payment.CfgAlipayMode, payment.ModePersonal)
	appID := payment.GetConfig(payment.CfgAlipayAppID, "")
	privRaw := payment.GetConfig(payment.CfgAlipayPrivateKey, "")
	pubRaw := payment.GetConfig(payment.CfgAlipayPublicKey, "")
	siteURL := payment.GetConfig(payment.CfgSiteURL, "")

	checks := []gin.H{}
	ok := true
	add := func(name string, pass bool, detail string) {
		if !pass {
			ok = false
		}
		checks = append(checks, gin.H{"name": name, "pass": pass, "detail": detail})
	}

	modeDetail := "当前为个人码模式，切换为「官方」后订单走开放平台"
	if mode == payment.ModeOfficial {
		modeDetail = "官方模式"
	}
	add("收款模式", mode == payment.ModeOfficial, modeDetail)
	if appID != "" {
		add("AppID", true, appID)
	} else {
		add("AppID", false, "未配置")
	}
	if privRaw != "" {
		add("应用私钥", true, "已配置")
	} else {
		add("应用私钥", false, "未配置")
	}
	if pubRaw != "" {
		add("支付宝公钥", true, "已配置（用于回调验签）")
	} else {
		add("支付宝公钥", false, "未配置（回调将无法验签）")
	}
	if siteURL != "" {
		add("站点 URL", true, siteURL)
	} else {
		add("站点 URL", false, "未配置 site_url，官方回调地址无法生成")
	}

	if appID == "" || privRaw == "" {
		c.JSON(http.StatusOK, gin.H{"code": -1, "msg": "请先配置 AppID 与应用私钥", "data": gin.H{"ok": false, "checks": checks}})
		return
	}

	// 调用支付宝开放接口：查询不存在的订单，用返回码判断鉴权是否通过
	result, err := payment.TestAlipayCredentials()
	if err != nil {
		add("开放平台连通", false, err.Error())
		c.JSON(http.StatusOK, gin.H{"code": -1, "msg": "连通测试失败: " + err.Error(), "data": gin.H{"ok": false, "checks": checks}})
		return
	}
	add("开放平台连通", result.AuthOK, result.Message)

	msg := "支付宝官方配置可用"
	if !ok || !result.AuthOK {
		msg = "配置存在问题，请检查上方检测项"
		ok = false
	}
	code := 1
	if !ok {
		code = -1
	}
	c.JSON(http.StatusOK, gin.H{
		"code": code,
		"msg":  msg,
		"data": gin.H{
			"ok":          ok && result.AuthOK,
			"checks":      checks,
			"gateway":     result.Gateway,
			"alipay_code": result.Code,
			"notify_url":  strings.TrimRight(siteURL, "/") + "/api/channel/notify/alipay",
		},
	})
}

// TestWechatConfig 测试官方微信商户配置（查询不存在订单验证 APIv3 证书鉴权）
func (h *OfficialPayHandler) TestWechatConfig(c *gin.Context) {
	mode := payment.GetConfig(payment.CfgWechatMode, payment.ModePersonal)
	appID := payment.GetConfig(payment.CfgWechatAppID, "")
	mchID := payment.GetConfig(payment.CfgWechatMchID, "")
	serial := payment.GetConfig(payment.CfgWechatSerialNo, "")
	privRaw := payment.GetConfig(payment.CfgWechatPrivateKey, "")
	apiV3Key := payment.GetConfig(payment.CfgWechatAPIv3Key, "")
	siteURL := payment.GetConfig(payment.CfgSiteURL, "")

	checks := []gin.H{}
	ok := true
	add := func(name string, pass bool, detail string) {
		if !pass {
			ok = false
		}
		checks = append(checks, gin.H{"name": name, "pass": pass, "detail": detail})
	}

	modeDetail := "当前为个人码模式，切换为「官方」后订单走微信支付 APIv3"
	if mode == payment.ModeOfficial {
		modeDetail = "官方商户模式 (Native 扫码)"
	}
	add("收款模式", mode == payment.ModeOfficial, modeDetail)

	if appID != "" {
		add("AppID", true, appID)
	} else {
		add("AppID", false, "未配置")
	}
	if mchID != "" {
		add("商户号 MchID", true, mchID)
	} else {
		add("商户号 MchID", false, "未配置")
	}
	if serial != "" {
		add("证书序列号", true, serial)
	} else {
		add("证书序列号", false, "未配置（商户 API 证书序列号）")
	}
	if privRaw != "" {
		add("商户 API 私钥", true, "已配置")
	} else {
		add("商户 API 私钥", false, "未配置")
	}
	if len(apiV3Key) == 32 {
		add("APIv3 密钥", true, "已配置（32 位）")
	} else if apiV3Key != "" {
		add("APIv3 密钥", false, fmt.Sprintf("长度错误：当前 %d，需 32 位", len(apiV3Key)))
	} else {
		add("APIv3 密钥", false, "未配置（用于回调解密）")
	}
	if siteURL != "" {
		add("站点 URL", true, siteURL)
	} else {
		add("站点 URL", false, "未配置 site_url，官方回调地址无法生成")
	}

	if !payment.WechatConfigComplete() {
		c.JSON(http.StatusOK, gin.H{
			"code": -1,
			"msg":  "请先配齐 AppID、商户号、证书序列号、API 私钥、APIv3 密钥",
			"data": gin.H{"ok": false, "checks": checks},
		})
		return
	}

	result, err := payment.TestWechatCredentials()
	if err != nil {
		add("商户平台连通", false, err.Error())
		c.JSON(http.StatusOK, gin.H{
			"code": -1,
			"msg":  "连通测试失败: " + err.Error(),
			"data": gin.H{"ok": false, "checks": checks},
		})
		return
	}
	add("商户平台连通", result.AuthOK, result.Message)

	msg := "微信官方商户配置可用"
	if !ok || !result.AuthOK {
		msg = "配置存在问题，请检查上方检测项"
		ok = false
	}
	code := 1
	if !ok {
		code = -1
	}
	c.JSON(http.StatusOK, gin.H{
		"code": code,
		"msg":  msg,
		"data": gin.H{
			"ok":         ok && result.AuthOK,
			"checks":     checks,
			"gateway":    result.Gateway,
			"http":       result.HTTP,
			"wechat_code": result.Code,
			"notify_url": strings.TrimRight(siteURL, "/") + "/api/channel/notify/wechat",
		},
	})
}
