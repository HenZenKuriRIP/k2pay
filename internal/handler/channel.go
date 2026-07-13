package handler

import (
	"log"
	"net/http"

	"github.com/HenZenKuriRIP/k2pay/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

// ChannelHandler 上游通道回调处理器
type ChannelHandler struct{}

// NewChannelHandler 创建处理器
func NewChannelHandler() *ChannelHandler {
	return &ChannelHandler{}
}

// EpayNotify 处理上游易支付回调
// GET /channel/notify/epay
func (h *ChannelHandler) EpayNotify(c *gin.Context) {
	params := map[string]string{}
	for k, vs := range c.Request.URL.Query() {
		if len(vs) > 0 {
			params[k] = vs[0]
		}
	}

	tradeNo := params["out_trade_no"] // 我们传给上游的是本地 trade_no
	if tradeNo == "" {
		tradeNo = params["trade_no"]
	}
	upstreamTradeNo := params["trade_no"]
	tradeStatus := params["trade_status"]
	money := params["money"]
	sign := params["sign"]

	log.Printf("Epay upstream notify: local=%s upstream=%s status=%s", tradeNo, upstreamTradeNo, tradeStatus)

	if tradeStatus != "" && tradeStatus != "TRADE_SUCCESS" {
		c.String(http.StatusOK, "fail")
		return
	}

	channelService := service.GetChannelService()
	cfg, err := channelService.GetChannelConfig(service.ChannelTypeEpay)
	if err != nil {
		log.Printf("Epay channel not configured: %v", err)
		c.String(http.StatusOK, "fail")
		return
	}

	if !channelService.VerifyEpayNotify(cfg, params, sign) {
		log.Printf("Epay notify sign verify failed")
		c.String(http.StatusOK, "fail")
		return
	}

	amount, _ := decimal.NewFromString(money)
	if err := channelService.HandleUpstreamNotify(service.ChannelTypeEpay, tradeNo, upstreamTradeNo, amount); err != nil {
		log.Printf("Handle epay notify failed: %v", err)
		c.String(http.StatusOK, "fail")
		return
	}

	c.String(http.StatusOK, "success")
}
