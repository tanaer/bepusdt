package epusdt

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/v03413/bepusdt/app/log"
	"github.com/v03413/bepusdt/app/model"
	"github.com/v03413/bepusdt/app/task"
	"github.com/v03413/bepusdt/app/utils"
)

type Epusdt struct{}

type createReq struct {
	OrderID     string     `json:"order_id" binding:"required"`
	NotifyURL   string     `json:"notify_url" binding:"required"`
	RedirectURL string     `json:"redirect_url" binding:"required"`
	Signature   string     `json:"signature" binding:"required"`
	Amount      float64    `json:"amount"`
	Name        string     `json:"name"`
	Fiat        model.Fiat `json:"fiat"`
	TradeType   string     `json:"trade_type"`
	Address     string     `json:"address"`
	Timeout     int64      `json:"timeout"`
	Rate        string     `json:"rate"`
}

type createOrderReq struct {
	OrderID     string     `json:"order_id" binding:"required"`
	NotifyURL   string     `json:"notify_url" binding:"required"`
	RedirectURL string     `json:"redirect_url" binding:"required"`
	Signature   string     `json:"signature" binding:"required"`
	Amount      float64    `json:"amount"`
	Name        string     `json:"name"`
	Fiat        model.Fiat `json:"fiat"`
	Currencies  string     `json:"currencies"`
	Timeout     int64      `json:"timeout"`
	Reselect    *bool      `json:"reselect"`
}

type updateOrderReq struct {
	TradeID  string `json:"trade_id" binding:"required"`
	Currency string `json:"currency" binding:"required"`
	Network  string `json:"network" binding:"required"`
}

type cancelReq struct {
	TradeID   string `json:"trade_id" binding:"required"`
	Signature string `json:"signature" binding:"required"`
}

type infoReq struct {
	TradeID string `json:"trade_id" binding:"required"`
}

type methodsReq struct {
	TradeID  string `json:"trade_id" binding:"required"`
	Currency string `json:"currency"`
}

type verifyTransactionReq struct {
	TradeID string `json:"trade_id" binding:"required"`
	TxHash  string `json:"tx_hash" binding:"required"`
}

var verifyAndClaimSubmittedPayment = task.VerifyAndClaimSubmittedPayment

// tradeTypeReselect 返回本次 create-order 是否允许确认交易类型后再次重选；未传 reselect 时使用后台全局配置。
func (r createOrderReq) tradeTypeReselect() bool {
	if r.Reselect == nil {
		return model.OrderTradeTypeReselectEnabled()
	}

	return *r.Reselect
}

func (Epusdt) Notify(ctx *gin.Context) {
	rawData, err := ctx.GetRawData()
	if err != nil {
		ctx.String(200, "fail")
		return
	}

	m := make(map[string]any)
	if err = json.Unmarshal(rawData, &m); err != nil {
		ctx.String(200, "fail")
		return
	}

	sign, ok := m["signature"]
	if !ok {
		ctx.String(200, "fail")
		return
	}

	if utils.EpusdtSign(m, model.AuthToken()) != sign {
		ctx.String(200, "fail")
		return
	}

	ctx.String(200, "ok")
}

func (Epusdt) CreateOrder(ctx *gin.Context) {
	var req createOrderReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(200, respFailJson(fmt.Sprintf("CreateOrder: request error: %s", err.Error())))

		return
	}

	if !utils.IsAllowedCallbackURL(req.NotifyURL) {
		ctx.JSON(200, respFailJson("notify_url 地址不合法"))

		return
	}
	if !utils.IsAllowedCallbackURL(req.RedirectURL) {
		ctx.JSON(200, respFailJson("redirect_url 地址不合法"))

		return
	}

	// 解析请求地址
	host := "http://" + ctx.Request.Host
	if ctx.Request.TLS != nil {
		host = "https://" + ctx.Request.Host
	}

	if req.Fiat == "" {
		req.Fiat = model.CNY
	}

	// 创建待付款订单
	order, err := model.BuildPendingOrder(model.OrderParams{
		Money:             decimal.NewFromFloat(req.Amount),
		ApiType:           model.OrderApiTypeEpusdtOrder,
		OrderId:           req.OrderID,
		RedirectUrl:       req.RedirectURL,
		NotifyUrl:         req.NotifyURL,
		Name:              req.Name,
		Timeout:           req.Timeout,
		Fiat:              req.Fiat,
		CurrencyLimit:     req.Currencies,
		TradeTypeReselect: req.tradeTypeReselect(),
	})
	if err != nil {
		ctx.JSON(200, respFailJson(fmt.Sprintf("CreateOrder: order create failed: %s", err.Error())))
		return
	}

	log.Info(fmt.Sprintf("订单创建成功 商户订单：%s", req.OrderID))

	// 返回响应数据
	ctx.JSON(200, respSuccJson(gin.H{
		"fiat":            order.Fiat,
		"trade_id":        order.TradeId,
		"order_id":        order.OrderId,
		"name":            order.Name,
		"status":          order.Status,
		"amount":          order.Money,
		"expiration_time": uint64(order.ExpiredAt.Sub(time.Now()).Seconds()),
		"payment_url":     model.CheckoutUrl(host, order.TradeId),
		"network":         order.GetMethods(""),
		"reselect":        order.CanReselectPayment(),
	}))
}

func (Epusdt) UpdateOrder(ctx *gin.Context) {
	// 接收 trade_id, currency, network 三个参数
	var req updateOrderReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(200, respFailJson(fmt.Sprintf("UpdateOrder: request error: %s", err.Error())))
		return
	}

	// 解析请求地址
	host := "http://" + ctx.Request.Host
	if ctx.Request.TLS != nil {
		host = "https://" + ctx.Request.Host
	}

	// 获取订单
	order, ok := model.GetTradeOrder(req.TradeID)
	if !ok {
		ctx.JSON(200, respFailJson("order not found"))
		return
	}

	// 仅待付订单才可更新订单
	if order.Status != model.OrderStatusWaiting {
		ctx.JSON(200, respFailJson("update order failed: order status does not allow payment updates"))
		return
	}

	if order.TradeType != "" && !order.CanReselectPayment() {
		ctx.JSON(200, respFailJson("update order failed: order status does not allow payment updates"))
		return
	}

	// 拒绝任务未及时改成 expired，实际订单已过期时更新订单
	remaining := time.Until(order.ExpiredAt)
	if remaining <= 0 {
		ctx.JSON(200, respFailJson("update order failed: order expired"))
		return
	}

	// 根据 currency 和 network 解析出 TradeType
	tradeType, err := model.GetTradeTypeByCurrencyAndNetwork(req.Currency, req.Network)
	if err != nil {
		ctx.JSON(200, respFailJson(fmt.Sprintf("UpdateOrder: unsupported payment method: %s - %s", req.Currency, req.Network)))
		return
	}

	// 重建订单（更新支付方式）
	// 注意：RebuildOrder 需要 OrderParams，我们需要从现有订单构造参数
	money, _ := decimal.NewFromString(order.Money)
	params := model.OrderParams{
		Money:             money,
		OrderId:           order.OrderId,
		TradeType:         tradeType, // 新的交易类型
		RedirectUrl:       order.ReturnUrl,
		NotifyUrl:         order.NotifyUrl,
		Name:              order.Name,
		Timeout:           int64(math.Ceil(remaining.Seconds())),
		Fiat:              order.Fiat,
		TradeTypeReselect: order.TradeTypeReselect,
	}

	newOrder, err := model.RebuildOrder(order, params)
	if err != nil {
		ctx.JSON(200, respFailJson(fmt.Sprintf("update order failed: %s", err.Error())))
		return
	}

	// 返回响应数据
	ctx.JSON(200, respSuccJson(gin.H{
		"fiat":            newOrder.Fiat,
		"trade_type":      newOrder.TradeType,
		"trade_id":        newOrder.TradeId,
		"order_id":        newOrder.OrderId,
		"status":          newOrder.Status,
		"amount":          newOrder.Money,
		"actual_amount":   newOrder.Amount,
		"token":           newOrder.Address,
		"expiration_time": uint64(newOrder.ExpiredAt.Sub(time.Now()).Seconds()),
		"payment_url":     model.CheckoutUrl(host, newOrder.TradeId),
	}))
}

func (Epusdt) CreateTransaction(ctx *gin.Context) {
	var req createReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(200, respFailJson(fmt.Sprintf("请求参数错误：%s", err.Error())))

		return
	}

	if !utils.IsAllowedCallbackURL(req.NotifyURL) {
		ctx.JSON(200, respFailJson("notify_url 地址不合法"))

		return
	}
	if !utils.IsAllowedCallbackURL(req.RedirectURL) {
		ctx.JSON(200, respFailJson("redirect_url 地址不合法"))

		return
	}

	if req.Fiat == "" {
		req.Fiat = model.CNY
	}
	if req.TradeType == "" {
		req.TradeType = string(model.UsdtTrc20)
	}

	order, err := model.StartBuildOrder(model.OrderParams{
		Money:         decimal.NewFromFloat(req.Amount),
		ApiType:       model.OrderApiTypeEpusdt,
		Address:       req.Address,
		AddressLocked: req.Amount == 0,
		OrderId:       req.OrderID,
		TradeType:     model.TradeType(req.TradeType),
		RedirectUrl:   req.RedirectURL,
		NotifyUrl:     req.NotifyURL,
		Name:          req.Name,
		Timeout:       req.Timeout,
		Rate:          req.Rate,
		Fiat:          req.Fiat,
	})
	if err != nil {
		ctx.JSON(200, respFailJson(fmt.Sprintf("订单创建失败：%s", err.Error())))

		return
	}

	log.Info(fmt.Sprintf("订单创建成功 商户订单：%s", req.OrderID))

	// 返回响应数据
	ctx.JSON(200, respSuccJson(gin.H{
		"fiat":            order.Fiat,
		"trade_type":      order.TradeType,
		"trade_id":        order.TradeId,
		"order_id":        order.OrderId,
		"status":          order.Status,
		"amount":          order.Money,
		"actual_amount":   order.Amount,
		"token":           order.Address,
		"expiration_time": uint64(order.ExpiredAt.Sub(time.Now()).Seconds()),
		"payment_url":     model.CheckoutUrl(utils.GetRequestHost(ctx.Request), order.TradeId),
	}))
}

func (Epusdt) CancelTransaction(ctx *gin.Context) {
	var req cancelReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(200, respFailJson(fmt.Sprintf("请求参数错误：%s", err.Error())))

		return
	}

	order, ok := model.GetTradeOrder(req.TradeID)
	if !ok {
		ctx.JSON(200, respFailJson("订单不存在"))

		return
	}

	if order.Status != model.OrderStatusWaiting {
		ctx.JSON(200, respFailJson(fmt.Sprintf("当前订单(%s)状态不允许取消", req.TradeID)))

		return
	}

	if err := order.SetCanceled(); err != nil {
		ctx.JSON(200, respFailJson(fmt.Sprintf("订单取消失败：%s", err.Error())))

		return
	}

	ctx.JSON(200, respSuccJson(gin.H{"trade_id": req.TradeID}))
}

func (Epusdt) Checkout(ctx *gin.Context) {
	tradeId := ctx.Param("trade_id")
	if _, ok := model.GetTradeOrder(tradeId); !ok {
		ctx.String(200, "order not found")

		return
	}

	// 收银台模板
	tmpl := model.GetC(model.PaymentCheckout) + "/checkout.html"

	ctx.HTML(200, tmpl, gin.H{"trade_id": tradeId})
}

func (Epusdt) GetMethods(ctx *gin.Context) {
	var req methodsReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(200, respFailJson(fmt.Sprintf("request error: %s", err.Error())))
		return
	}

	order, ok := model.GetTradeOrder(req.TradeID)
	if !ok {
		ctx.JSON(200, respFailJson("order not found"))
		return
	}

	if order.Status != model.OrderStatusWaiting {
		ctx.JSON(200, respFailJson("error: Invalid order status"))
		return
	}

	if order.TradeType != "" && !order.CanReselectPayment() {
		ctx.JSON(200, respFailJson("error: The order status does not allow retrieving the payment method"))
		return
	}

	ctx.JSON(200, respSuccJson(gin.H{
		"methods": order.GetMethods(model.Crypto(req.Currency)),
	}))
}

func (Epusdt) Info(ctx *gin.Context) {
	var req infoReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(200, respFailJson(fmt.Sprintf("参数错误：%s", err.Error())))
		return
	}

	order, ok := model.GetTradeOrder(req.TradeID)
	if !ok {
		ctx.JSON(200, respFailJson("订单不存在"))
		return
	}

	var returnURL string
	if order.Status == model.OrderStatusSuccess {
		returnURL = order.ReturnUrl
		if order.ApiType == model.OrderApiTypeEpay {
			returnURL = fmt.Sprintf("%s?%s", returnURL, order.BuildNotifyParams())
		}
	}
	progress := model.GetChainProgress(order)

	ctx.JSON(200, respSuccJson(gin.H{
		"network":                order.Network(),
		"trade_id":               order.TradeId,
		"order_id":               order.OrderId,
		"trade_type":             order.TradeType,
		"status":                 order.Status,
		"money":                  order.Money,
		"actual_amount":          order.Amount,
		"token":                  order.Address,
		"fiat":                   order.Fiat,
		"name":                   order.Name,
		"expired_at":             order.ExpiredAt.Unix(),
		"created_at":             order.CreatedAt.Time().Unix(),
		"trade_url":              order.GetTxUrl(),
		"support_url":            model.GetC(model.PaymentSupportUrl),
		"redirect_url":           order.RedirectUrl(),
		"return_url":             returnURL,
		"trade_hash":             order.RefHash,
		"confirmations":          progress.Current,
		"required_confirmations": progress.Required,
		"reselect":               order.CanReselectPayment(),
	}))
}

// VerifyTransaction lets a payer submit an on-chain transaction hash as a
// verification clue. The task layer independently fetches and validates the
// transaction before this endpoint can move an order into confirmation.
func (Epusdt) VerifyTransaction(ctx *gin.Context) {
	var req verifyTransactionReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(200, respFailJson("invalid transaction verification request"))
		return
	}

	order, ok := model.GetTradeOrder(req.TradeID)
	if !ok {
		ctx.JSON(200, respFailJson("order not found"))
		return
	}

	if (order.Status == model.OrderStatusConfirming || order.Status == model.OrderStatusSuccess) &&
		order.RefHash != "" && strings.EqualFold(order.RefHash, strings.TrimSpace(req.TxHash)) {
		ctx.JSON(200, respSuccJson(gin.H{
			"trade_id":   order.TradeId,
			"status":     order.Status,
			"trade_hash": order.RefHash,
			"idempotent": true,
		}))
		return
	}
	if order.Status != model.OrderStatusWaiting && order.Status != model.OrderStatusExpired {
		ctx.JSON(200, respFailJson("the current order status does not allow transaction verification"))
		return
	}

	_, err := verifyAndClaimSubmittedPayment(ctx.Request.Context(), &order, req.TxHash)
	if err != nil {
		ctx.JSON(200, respFailJson(submittedPaymentErrorMessage(err)))
		return
	}

	ctx.JSON(200, respSuccJson(gin.H{
		"trade_id":   order.TradeId,
		"status":     order.Status,
		"trade_hash": order.RefHash,
	}))
}

func submittedPaymentErrorMessage(err error) string {
	switch {
	case errors.Is(err, task.ErrInvalidSubmittedPaymentHash):
		return "invalid transaction hash"
	case errors.Is(err, task.ErrUnsupportedSubmittedPayment):
		return "transaction hash verification is not available for this payment network"
	case errors.Is(err, task.ErrSubmittedPaymentNotFound):
		return "transaction was not found or has not been confirmed on-chain yet"
	case errors.Is(err, task.ErrSubmittedPaymentDoesNotMatch):
		return "the submitted transaction does not match this order"
	case errors.Is(err, model.ErrPaymentHashAlreadyClaimed):
		return "this transaction has already been used for another order"
	case errors.Is(err, model.ErrOrderNoLongerReceivable):
		return "the current order status does not allow transaction verification"
	default:
		log.Warn(fmt.Sprintf("verify submitted transaction failed: %v", err))
		return "unable to verify this transaction right now; please try again"
	}
}

func (Epusdt) SignVerify(ctx *gin.Context) {
	rawData, err := ctx.GetRawData()
	if err != nil {
		ctx.JSON(200, respFailJson(fmt.Sprintf("json 数据读取错误 %s", err.Error())))
		ctx.Abort()

		return
	}

	m := make(map[string]any)
	if err = json.Unmarshal(rawData, &m); err != nil {
		ctx.JSON(200, respFailJson(fmt.Sprintf("json 数据解析错误 %s", err.Error())))
		ctx.Abort()

		return
	}

	sign, ok := m["signature"]
	if !ok {
		ctx.JSON(200, respFailJson("签名丢失"))
		ctx.Abort()

		return
	}

	if utils.EpusdtSign(m, model.AuthToken()) != sign {
		ctx.JSON(200, respFailJson("签名错误"))
		ctx.Abort()

		return
	}

	ctx.Request.Body = io.NopCloser(bytes.NewBuffer(rawData)) // 回写数据
	ctx.Next()
}

func respFailJson(message string) gin.H {

	return gin.H{"status_code": 400, "message": message}
}

func respSuccJson(data interface{}) gin.H {
	return gin.H{"status_code": 200, "message": "success", "data": data, "request_id": ""}
}
