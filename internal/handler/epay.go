package handler

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/HenZenKuriRIP/k2pay/internal/middleware"
	"github.com/HenZenKuriRIP/k2pay/internal/model"
	"github.com/HenZenKuriRIP/k2pay/internal/payment"
	"github.com/HenZenKuriRIP/k2pay/internal/service"
	"github.com/HenZenKuriRIP/k2pay/internal/util"

	"github.com/gin-gonic/gin"
)

// EpayHandler 彩虹易支付兼容接口处理器
type EpayHandler struct{}

// NewEpayHandler 创建处理器
func NewEpayHandler() *EpayHandler {
	return &EpayHandler{}
}

// absolutePayURL 生成绝对支付地址
func absolutePayURL(c *gin.Context, path string) string {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	scheme := "http"
	if c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := c.Request.Host
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return scheme + "://" + host + path
}

// collectPayParams 收集支付参数
func collectPayParams(c *gin.Context) map[string]string {
	keys := []string{
		"pid", "type", "out_trade_no", "notify_url", "return_url",
		"name", "money", "currency", "param", "sign", "sign_type", "clientip",
	}
	params := make(map[string]string)
	for _, k := range keys {
		v := c.DefaultQuery(k, c.PostForm(k))
		if v != "" {
			params[k] = v
		}
	}
	return params
}

// Submit 发起支付 (submit.php 兼容)
func (h *EpayHandler) Submit(c *gin.Context) {
	raw := collectPayParams(c)
	pid := raw["pid"]
	payType := raw["type"]
	outTradeNo := raw["out_trade_no"]
	notifyURL := raw["notify_url"]
	returnURL := raw["return_url"]
	name := raw["name"]
	money := raw["money"]
	currency := raw["currency"]
	sign := raw["sign"]
	param := raw["param"]

	if pid == "" || outTradeNo == "" || money == "" || sign == "" {
		middleware.SetAPILogContext(c, -4, "参数不完整", "", 0, pid)
		h.renderError(c, "参数不完整")
		return
	}
	if name == "" {
		name = "在线支付"
	}

	merchant, err := h.loadMerchant(pid)
	if err != nil {
		middleware.SetAPILogContext(c, -3, err.Error(), "", 0, pid)
		h.renderError(c, err.Error())
		return
	}
	if msg := h.checkMerchantAccess(c, merchant); msg != "" {
		middleware.SetAPILogContext(c, -1, msg, "", merchant.ID, pid)
		h.renderError(c, msg)
		return
	}

	signParams := map[string]string{
		"pid": pid, "type": payType, "out_trade_no": outTradeNo,
		"notify_url": notifyURL, "return_url": returnURL,
		"name": name, "money": money, "currency": currency, "param": param,
	}
	if !util.VerifySign(signParams, merchant.Key, sign) {
		log.Printf("[Submit] 签名验证失败, pid=%s, out_trade_no=%s", pid, outTradeNo)
		middleware.SetAPILogContext(c, -3, "签名验证失败", "", merchant.ID, pid)
		h.renderError(c, "MD5签名校验失败")
		return
	}

	if notifyURL == "" {
		notifyURL = merchant.NotifyURL
	}
	if returnURL == "" {
		returnURL = merchant.ReturnURL
	}
	if currency == "" {
		currency = "CNY"
	}

	resp, err := service.GetOrderService().CreateOrder(&service.CreateOrderRequest{
		MerchantPID: pid,
		Type:        payType,
		OutTradeNo:  outTradeNo,
		NotifyURL:   notifyURL,
		ReturnURL:   returnURL,
		Name:        name,
		Money:       money,
		Currency:    currency,
		Param:       param,
		ClientIP:    c.ClientIP(),
	})
	if err != nil {
		middleware.SetAPILogContext(c, -1, err.Error(), "", merchant.ID, pid)
		h.renderError(c, err.Error())
		return
	}

	middleware.SetAPILogContext(c, 1, "success", resp.TradeNo, merchant.ID, pid)
	c.Redirect(http.StatusFound, "/cashier/"+resp.TradeNo)
}

// MAPISubmit API 方式发起支付 (mapi.php 兼容)
func (h *EpayHandler) MAPISubmit(c *gin.Context) {
	raw := collectPayParams(c)
	pid := raw["pid"]
	payType := raw["type"]
	outTradeNo := raw["out_trade_no"]
	notifyURL := raw["notify_url"]
	returnURL := raw["return_url"]
	name := raw["name"]
	money := raw["money"]
	currency := raw["currency"]
	sign := raw["sign"]
	param := raw["param"]
	clientIP := raw["clientip"]
	if clientIP == "" {
		clientIP = c.ClientIP()
	}

	if pid == "" || outTradeNo == "" || money == "" || sign == "" {
		middleware.SetAPILogContext(c, -4, "参数不完整", "", 0, pid)
		c.JSON(http.StatusOK, gin.H{"code": -4, "msg": "参数不完整"})
		return
	}
	if name == "" {
		name = "在线支付"
	}

	merchant, err := h.loadMerchant(pid)
	if err != nil {
		middleware.SetAPILogContext(c, -3, err.Error(), "", 0, pid)
		c.JSON(http.StatusOK, gin.H{"code": -3, "msg": err.Error()})
		return
	}
	if msg := h.checkMerchantAccess(c, merchant); msg != "" {
		middleware.SetAPILogContext(c, -1, msg, "", merchant.ID, pid)
		c.JSON(http.StatusOK, gin.H{"code": -1, "msg": msg})
		return
	}

	signParams := map[string]string{
		"pid": pid, "type": payType, "out_trade_no": outTradeNo,
		"notify_url": notifyURL, "return_url": returnURL,
		"name": name, "money": money, "currency": currency, "param": param,
		"clientip": clientIP,
	}
	// clientip 可能未参与部分商户签名，再试不含 clientip
	if !util.VerifySign(signParams, merchant.Key, sign) {
		delete(signParams, "clientip")
		if !util.VerifySign(signParams, merchant.Key, sign) {
			log.Printf("[MAPISubmit] 签名验证失败, pid=%s", pid)
			middleware.SetAPILogContext(c, -3, "签名验证失败", "", merchant.ID, pid)
			c.JSON(http.StatusOK, gin.H{"code": -3, "msg": "MD5签名校验失败"})
			return
		}
	}

	if notifyURL == "" {
		notifyURL = merchant.NotifyURL
	}
	if returnURL == "" {
		returnURL = merchant.ReturnURL
	}
	if currency == "" {
		currency = "CNY"
	}

	resp, err := service.GetOrderService().CreateOrder(&service.CreateOrderRequest{
		MerchantPID: pid,
		Type:        payType,
		OutTradeNo:  outTradeNo,
		NotifyURL:   notifyURL,
		ReturnURL:   returnURL,
		Name:        name,
		Money:       money,
		Currency:    currency,
		Param:       param,
		ClientIP:    clientIP,
	})
	if err != nil {
		middleware.SetAPILogContext(c, -1, err.Error(), "", merchant.ID, pid)
		c.JSON(http.StatusOK, gin.H{"code": -1, "msg": err.Error()})
		return
	}

	middleware.SetAPILogContext(c, 1, "success", resp.TradeNo, merchant.ID, pid)
	payURL := absolutePayURL(c, resp.PayURL)
	c.JSON(http.StatusOK, gin.H{
		"code":         1,
		"msg":          "success",
		"trade_no":     resp.TradeNo,
		"out_trade_no": resp.OutTradeNo,
		"type":         resp.Type,
		"money":        resp.Money,
		"payurl":       payURL, // 经典字段
		"pay_url":      payURL, // 兼容旧字段
		"qrcode":       resp.QRCode,
		// 加密货币扩展（非经典字段，保留便于 SDK 选用）
		"currency":     resp.Currency,
		"pay_currency": resp.PayCurrency,
		"pay_amount":   resp.PayAmount,
		"usdt_amount":  resp.USDTAmount,
		"rate":         resp.Rate,
		"address":      resp.Address,
		"chain":        resp.Chain,
		"expired_at":   resp.ExpiredAt,
	})
}

// API 商户查询类接口
// 路径: /api/pay/merchant|order|orders|settle|refund
// 亦支持 ?act=query|order|orders|settle|refund（语义对齐易支付）
func (h *EpayHandler) API(c *gin.Context) {
	act := c.Query("act")
	if act == "" {
		act = c.PostForm("act")
	}
	// 从路径末段推断
	if act == "" {
		path := strings.TrimSuffix(c.Request.URL.Path, "/")
		if i := strings.LastIndex(path, "/"); i >= 0 {
			act = path[i+1:]
		}
	}
	// 路径别名
	switch act {
	case "merchant", "query":
		act = "query"
	case "settles":
		act = "settle"
	case "pay", "api", "":
		act = ""
	}

	switch act {
	case "order":
		h.apiOrder(c)
	case "query":
		h.apiQueryMerchant(c)
	case "orders":
		h.apiOrders(c)
	case "settle":
		h.apiSettle(c)
	case "refund":
		h.apiRefund(c)
	default:
		c.JSON(http.StatusOK, gin.H{"code": -5, "msg": "unknown action, use merchant|order|orders|settle|refund"})
	}
}

// apiQueryMerchant act=query 商户信息
func (h *EpayHandler) apiQueryMerchant(c *gin.Context) {
	pid := c.Query("pid")
	key := c.Query("key")
	if pid == "" || key == "" {
		c.JSON(http.StatusOK, gin.H{"code": -4, "msg": "参数不完整"})
		return
	}
	var merchant model.Merchant
	if err := model.GetDB().Where("p_id = ?", pid).First(&merchant).Error; err != nil {
		c.JSON(http.StatusOK, gin.H{"code": -3, "msg": "商户ID不存在"})
		return
	}
	if merchant.Key != key {
		c.JSON(http.StatusOK, gin.H{"code": -3, "msg": "商户密钥错误"})
		return
	}

	var orders, ordersToday, ordersLastday int64
	model.GetDB().Model(&model.Order{}).Where("merchant_id = ?", merchant.ID).Count(&orders)
	today := time.Now().Format("2006-01-02")
	lastday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	model.GetDB().Model(&model.Order{}).
		Where("merchant_id = ? AND status = ? AND DATE(created_at) = ?", merchant.ID, model.OrderStatusPaid, today).
		Count(&ordersToday)
	model.GetDB().Model(&model.Order{}).
		Where("merchant_id = ? AND status = ? AND DATE(created_at) = ?", merchant.ID, model.OrderStatusPaid, lastday).
		Count(&ordersLastday)

	c.JSON(http.StatusOK, gin.H{
		"code":            1,
		"pid":             pid,
		"key":             key,
		"active":          merchant.Status,
		"money":           fmt.Sprintf("%.2f", merchant.Balance),
		"type":            1,
		"account":         "",
		"username":        merchant.Name,
		"orders":          orders,
		"orders_today":    ordersToday,
		"orders_lastday":  ordersLastday,
	})
}

// apiOrder act=order 查询单笔订单
func (h *EpayHandler) apiOrder(c *gin.Context) {
	pid := c.Query("pid")
	key := c.Query("key")
	outTradeNo := c.Query("out_trade_no")
	tradeNo := c.Query("trade_no")

	if pid == "" || key == "" {
		c.JSON(http.StatusOK, gin.H{"code": -4, "msg": "参数不完整"})
		return
	}
	var merchant model.Merchant
	if err := model.GetDB().Where("p_id = ?", pid).First(&merchant).Error; err != nil {
		c.JSON(http.StatusOK, gin.H{"code": -3, "msg": "商户ID不存在"})
		return
	}
	if merchant.Key != key {
		c.JSON(http.StatusOK, gin.H{"code": -3, "msg": "商户密钥错误"})
		return
	}

	orderService := service.GetOrderService()
	var order *model.Order
	var err error
	if tradeNo != "" {
		order, err = orderService.GetOrder(tradeNo)
		if err == nil && order.MerchantID != merchant.ID {
			err = fmt.Errorf("订单不存在")
		}
	} else if outTradeNo != "" {
		order, err = orderService.GetOrderByOutTradeNo(pid, outTradeNo)
	} else {
		c.JSON(http.StatusOK, gin.H{"code": -4, "msg": "订单号不能为空"})
		return
	}
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"code": -1, "msg": "订单号不存在"})
		return
	}

	// 经典 status: 0 未支付 1 已支付
	status := 0
	if order.Status == model.OrderStatusPaid {
		status = 1
	} else if order.Status == model.OrderStatusRefunded {
		status = 1 // 已支付后退款，查单仍可认为支付过；部分实现用 2
	}

	endtime := ""
	if order.PaidAt != nil {
		endtime = order.PaidAt.Format("2006-01-02 15:04:05")
	} else {
		endtime = order.ExpiredAt.Format("2006-01-02 15:04:05")
	}

	apiTradeNo := order.ApiTradeNo
	if apiTradeNo == "" {
		apiTradeNo = order.TxHash
	}

	c.JSON(http.StatusOK, gin.H{
		"code":         1,
		"msg":          "succ",
		"trade_no":     order.TradeNo,
		"out_trade_no": order.OutTradeNo,
		"api_trade_no": apiTradeNo,
		"bill_trade_no": "",
		"type":         util.ToEpayType(order.Type),
		"pid":          pid,
		"addtime":      order.CreatedAt.Format("2006-01-02 15:04:05"),
		"endtime":      endtime,
		"name":         order.Name,
		"money":        order.Money.StringFixed(2),
		"param":        order.Param,
		"buyer":        order.Buyer,
		"status":       status,
		"payurl":       absolutePayURL(c, "/cashier/"+order.TradeNo),
	})
}

// apiOrders act=orders 订单列表
func (h *EpayHandler) apiOrders(c *gin.Context) {
	pid := c.Query("pid")
	key := c.Query("key")
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "10"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	if offset < 0 {
		offset = 0
	}

	if pid == "" || key == "" {
		c.JSON(http.StatusOK, gin.H{"code": -4, "msg": "参数不完整"})
		return
	}
	var merchant model.Merchant
	if err := model.GetDB().Where("p_id = ?", pid).First(&merchant).Error; err != nil {
		c.JSON(http.StatusOK, gin.H{"code": -3, "msg": "商户ID不存在"})
		return
	}
	if merchant.Key != key {
		c.JSON(http.StatusOK, gin.H{"code": -3, "msg": "商户密钥错误"})
		return
	}

	db := model.GetDB().Model(&model.Order{}).Where("merchant_id = ?", merchant.ID)
	if statusStr := c.Query("status"); statusStr != "" {
		if st, err := strconv.Atoi(statusStr); err == nil {
			db = db.Where("status = ?", st)
		}
	}

	var orders []model.Order
	db.Order("id DESC").Offset(offset).Limit(limit).Find(&orders)

	data := make([]gin.H, 0, len(orders))
	for _, o := range orders {
		st := 0
		if o.Status == model.OrderStatusPaid || o.Status == model.OrderStatusRefunded {
			st = 1
		}
		endtime := o.ExpiredAt.Format("2006-01-02 15:04:05")
		if o.PaidAt != nil {
			endtime = o.PaidAt.Format("2006-01-02 15:04:05")
		}
		data = append(data, gin.H{
			"trade_no":     o.TradeNo,
			"out_trade_no": o.OutTradeNo,
			"type":         util.ToEpayType(o.Type),
			"pid":          pid,
			"addtime":      o.CreatedAt.Format("2006-01-02 15:04:05"),
			"endtime":      endtime,
			"name":         o.Name,
			"money":        o.Money.StringFixed(2),
			"param":        o.Param,
			"buyer":        o.Buyer,
			"status":       st,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"code":  1,
		"msg":   "查询订单记录成功！",
		"count": len(data),
		"data":  data,
	})
}

// apiSettle act=settle 结算记录（由提现记录适配）
func (h *EpayHandler) apiSettle(c *gin.Context) {
	pid := c.Query("pid")
	key := c.Query("key")
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "10"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limit <= 0 || limit > 50 {
		limit = 10
	}

	if pid == "" || key == "" {
		c.JSON(http.StatusOK, gin.H{"code": -4, "msg": "参数不完整"})
		return
	}
	var merchant model.Merchant
	if err := model.GetDB().Where("p_id = ?", pid).First(&merchant).Error; err != nil {
		c.JSON(http.StatusOK, gin.H{"code": -3, "msg": "商户ID不存在"})
		return
	}
	if merchant.Key != key {
		c.JSON(http.StatusOK, gin.H{"code": -3, "msg": "商户密钥错误"})
		return
	}

	var rows []model.Withdrawal
	model.GetDB().Where("merchant_id = ?", merchant.ID).
		Order("id DESC").Offset(offset).Limit(limit).Find(&rows)

	data := make([]gin.H, 0, len(rows))
	for _, w := range rows {
		data = append(data, gin.H{
			"id":     w.ID,
			"uid":    pid,
			"money":  fmt.Sprintf("%.2f", w.Amount),
			"fee":    fmt.Sprintf("%.2f", w.Fee),
			"account": w.Account,
			"status": w.Status,
			"result": w.AdminRemark,
			"addtime": w.CreatedAt.Format("2006-01-02 15:04:05"),
			"endtime": func() string {
				if w.ProcessedAt != nil {
					return w.ProcessedAt.Format("2006-01-02 15:04:05")
				}
				return ""
			}(),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 1,
		"msg":  "查询结算记录成功！",
		"data": data,
	})
}

// apiRefund act=refund 退款
func (h *EpayHandler) apiRefund(c *gin.Context) {
	pid := c.DefaultQuery("pid", c.PostForm("pid"))
	key := c.DefaultQuery("key", c.PostForm("key"))
	money := c.DefaultQuery("money", c.PostForm("money"))
	tradeNo := c.DefaultQuery("trade_no", c.PostForm("trade_no"))
	outTradeNo := c.DefaultQuery("out_trade_no", c.PostForm("out_trade_no"))

	if pid == "" || key == "" {
		c.JSON(http.StatusOK, gin.H{"code": -4, "msg": "参数不完整"})
		return
	}
	var merchant model.Merchant
	if err := model.GetDB().Where("p_id = ?", pid).First(&merchant).Error; err != nil {
		c.JSON(http.StatusOK, gin.H{"code": -3, "msg": "商户ID不存在"})
		return
	}
	if merchant.Key != key {
		c.JSON(http.StatusOK, gin.H{"code": -3, "msg": "商户密钥错误"})
		return
	}

	result, err := service.GetOrderService().RefundOrder(pid, tradeNo, outTradeNo, money)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"code": -1, "msg": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}

// CheckOrder 收银台轮询 GET /api/pay/check?trade_no=&token=
func (h *EpayHandler) CheckOrder(c *gin.Context) {
	tradeNo := c.Query("trade_no")
	token := c.Query("token")
	if tradeNo == "" {
		c.JSON(http.StatusOK, gin.H{"code": -1, "msg": "订单号不能为空"})
		return
	}
	if !util.VerifyOrderPollToken(tradeNo, token) {
		c.JSON(http.StatusOK, gin.H{"code": -1, "msg": "无效的轮询令牌"})
		return
	}

	paid, order, err := service.GetOrderService().CheckOrderPaid(tradeNo)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"code": -1, "msg": err.Error()})
		return
	}

	result := gin.H{"code": 1, "status": order.Status, "paid": paid}
	if paid && order.ReturnURL != "" {
		var merchant model.Merchant
		model.GetDB().First(&merchant, order.MerchantID)
		result["return_url"] = service.GetNotifyService().BuildReturnURL(order, &merchant)
	}
	c.JSON(http.StatusOK, result)
}

// SelectPaymentType 收银台选择支付方式
// POST /api/pay/:trade_no/select-type
func (h *EpayHandler) SelectPaymentType(c *gin.Context) {
	tradeNo := c.Param("trade_no")
	token := c.Query("token")
	if token == "" {
		token = c.PostForm("token")
	}
	var body struct {
		Type  string `json:"type"`
		Token string `json:"token"`
	}
	_ = c.ShouldBindJSON(&body)
	if token == "" && body.Token != "" {
		token = body.Token
	}
	if !util.VerifyOrderPollToken(tradeNo, token) {
		c.JSON(http.StatusOK, gin.H{"code": -1, "msg": "无效的令牌"})
		return
	}
	payType := c.DefaultQuery("type", c.PostForm("type"))
	if payType == "" {
		payType = body.Type
	}
	if payType == "" {
		c.JSON(http.StatusOK, gin.H{"code": -1, "msg": "请选择支付方式"})
		return
	}

	resp, err := service.GetOrderService().SelectPaymentType(tradeNo, payType)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"code": -1, "msg": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"code": 1,
		"msg":  "success",
		"data": resp,
	})
}

func (h *EpayHandler) loadMerchant(pid string) (*model.Merchant, error) {
	var merchant model.Merchant
	if err := model.GetDB().Where("p_id = ? AND status = 1", pid).First(&merchant).Error; err != nil {
		return nil, fmt.Errorf("商户不存在或已禁用")
	}
	return &merchant, nil
}

func (h *EpayHandler) checkMerchantAccess(c *gin.Context, merchant *model.Merchant) string {
	// 启用后必须配置名单，避免“开启但为空=全放行”的误配置
	if merchant.IPWhitelistEnabled {
		if strings.TrimSpace(merchant.IPWhitelist) == "" {
			return "IP白名单已启用但未配置任何IP"
		}
		if !middleware.CheckIPWhitelist(c.ClientIP(), merchant.IPWhitelist) {
			return "IP不在白名单内"
		}
	}
	if merchant.RefererWhitelistEnabled {
		if strings.TrimSpace(merchant.RefererWhitelist) == "" {
			return "Referer白名单已启用但未配置任何域名"
		}
		if !middleware.CheckRefererWhitelist(c.GetHeader("Referer"), merchant.RefererWhitelist) {
			return "请求来源不在白名单内"
		}
	}
	return ""
}

func (h *EpayHandler) renderError(c *gin.Context, msg string) {
	accept := c.GetHeader("Accept")
	if strings.Contains(accept, "application/json") {
		c.JSON(http.StatusOK, gin.H{"code": -1, "msg": msg})
		return
	}
	c.HTML(http.StatusOK, "error.html", gin.H{"title": "支付错误", "msg": msg})
}

// GetPaymentTypes 获取支持的支付类型
func (h *EpayHandler) GetPaymentTypes(c *gin.Context) {
	pid := c.Query("pid")
	if pid == "" {
		// 无 pid 时返回全部类型定义
		all := util.AllPaymentTypes()
		data := make([]gin.H, 0, len(all))
		for _, pt := range all {
			data = append(data, gin.H{
				"type": pt.EpayType, "name": pt.Name, "chain": pt.Chain,
				"enabled": true,
			})
		}
		c.JSON(http.StatusOK, gin.H{"code": 1, "msg": "success", "data": data})
		return
	}

	merchant, err := h.loadMerchant(pid)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"code": -1, "msg": err.Error()})
		return
	}

	// 按商户可用钱包过滤
	availableChains := make(map[string]bool)
	var wallets []model.Wallet
	switch merchant.WalletMode {
	case 1:
		model.GetDB().Where("(merchant_id IS NULL OR merchant_id = 0) AND status = 1").Find(&wallets)
	case 2:
		model.GetDB().Where("merchant_id = ? AND status = 1", merchant.ID).Find(&wallets)
	default:
		model.GetDB().Where("((merchant_id IS NULL OR merchant_id = 0) OR merchant_id = ?) AND status = 1", merchant.ID).Find(&wallets)
	}
	for _, w := range wallets {
		availableChains[w.Chain] = true
	}

	// 链监控启用状态；法币官方通道不依赖个人钱包
	chainStatus := service.GetBlockchainService().GetListenerStatus()
	isChainEnabled := func(chain string) bool {
		if chain == "wechat" || chain == "alipay" {
			if availableChains[chain] {
				return true
			}
			// 官方模式已配置则视为可用
			if d, err := payment.OfficialDriverForChain(chain); err == nil && d.Enabled() {
				return true
			}
			return false
		}
		if status, ok := chainStatus[chain]; ok {
			if s, ok := status.(map[string]interface{}); ok {
				if enabled, _ := s["enabled"].(bool); enabled {
					return availableChains[chain]
				}
			}
		}
		return false
	}

	var enabled []gin.H
	for _, pt := range util.AllPaymentTypes() {
		en := isChainEnabled(pt.Chain)
		enabled = append(enabled, gin.H{
			"type": pt.EpayType, "name": pt.Name, "chain": pt.Chain, "enabled": en,
		})
	}
	c.JSON(http.StatusOK, gin.H{"code": 1, "msg": "success", "data": enabled})
}
