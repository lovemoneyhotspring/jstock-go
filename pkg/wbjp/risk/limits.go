package risk

import (
	"fmt"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/marketrules"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/shopspring/decimal"
)

type RiskDecision struct {
	Approved bool
	Reason   string
}

type RiskContext struct {
	Equity           decimal.Decimal
	Balance          domain.Balance
	Positions        map[string]domain.Position
	BasePrices       map[string]decimal.Decimal
	PendingValue     map[string]decimal.Decimal
	OrdersToday      int
	RealizedPnLToday decimal.Decimal
}

type RiskManager struct {
	config    wbjpcfg.RiskConfig
	allowlist map[string]struct{}
}

func NewRiskManager(config wbjpcfg.RiskConfig, allowlist []string) *RiskManager {
	al := make(map[string]struct{})
	for _, s := range allowlist {
		al[s] = struct{}{}
	}
	return &RiskManager{
		config:    config,
		allowlist: al,
	}
}

func (r *RiskManager) Check(req domain.OrderRequest, ctx RiskContext, preview *domain.OrderPreview) RiskDecision {
	// 1. キルスイッチ
	if r.config.KillSwitch {
		return RiskDecision{Approved: false, Reason: "キルスイッチが有効（risk.kill_switch = true）"}
	}

	// 2. Allowlist
	if _, ok := r.allowlist[req.Symbol]; !ok {
		return RiskDecision{Approved: false, Reason: fmt.Sprintf("%s は allowlist（universe.symbols）に含まれていない", req.Symbol)}
	}

	// 3. 1日発注件数
	if ctx.OrdersToday >= r.config.MaxOrdersPerDay {
		return RiskDecision{Approved: false, Reason: fmt.Sprintf("当日の発注件数が上限 %d 件に達している", r.config.MaxOrdersPerDay)}
	}

	// 4. 当日最大損失（買い新規建てのみ制限）
	if req.Side == domain.SideBuy {
		loss := ctx.RealizedPnLToday.Neg()
		if loss.GreaterThanOrEqual(r.config.MaxDailyLoss) {
			return RiskDecision{Approved: false, Reason: fmt.Sprintf("当日の損失 %s 円が上限 %s 円に達している", loss, r.config.MaxDailyLoss)}
		}
	}

	// 5. 1注文あたりの約定代金上限（買いのみ）
	if req.Side == domain.SideBuy {
		notional := r.notional(req, ctx)
		if notional.GreaterThan(r.config.MaxOrderValue) {
			return RiskDecision{Approved: false, Reason: fmt.Sprintf("約定代金 %s 円が1注文の上限 %s 円を超える", notional, r.config.MaxOrderValue)}
		}
	}

	// 6. 値幅制限チェック
	if req.LimitPrice != nil {
		if base, ok := ctx.BasePrices[req.Symbol]; ok && base.GreaterThan(decimal.Zero) {
			within, err := marketrules.IsWithinPriceLimit(*req.LimitPrice, base)
			if err == nil && !within {
				return RiskDecision{Approved: false, Reason: fmt.Sprintf("指値 %s が基準値段 %s の値幅制限を外れている", req.LimitPrice, base)}
			}
		}
	}

	// 7. 売却可能数量チェック（売りのみ）
	if req.Side == domain.SideSell && req.Trade != domain.TradeTypeMarginOpen {
		pos, ok := ctx.Positions[req.Symbol]
		if !ok || pos.AvailableQuantity.LessThan(req.Quantity) {
			avail := decimal.Zero
			if ok {
				avail = pos.AvailableQuantity
			}
			return RiskDecision{Approved: false, Reason: fmt.Sprintf("%s: 売却可能数量 %s に対し %s 株の売り", req.Symbol, avail, req.Quantity)}
		}
	}

	// 8. 買付余力チェック（買いのみ）
	if req.Side == domain.SideBuy {
		notional := r.notional(req, ctx)
		if notional.GreaterThan(ctx.Balance.BuyingPower) {
			return RiskDecision{Approved: false, Reason: fmt.Sprintf("%s: 買付余力 %s 円に対し必要概算 %s 円", req.Symbol, ctx.Balance.BuyingPower, notional)}
		}
	}

	// 9. ポートフォリオ比率上限（買いのみ）
	if req.Side == domain.SideBuy && ctx.Equity.GreaterThan(decimal.Zero) {
		currentVal := decimal.Zero
		if pos, ok := ctx.Positions[req.Symbol]; ok {
			currentVal = pos.MarketValue()
		}
		pending := ctx.PendingValue[req.Symbol]
		newNotional := r.notional(req, ctx)
		totalAfter := currentVal.Add(pending).Add(newNotional)
		weight := totalAfter.Div(ctx.Equity)

		if weight.GreaterThan(r.config.MaxPositionWeight) {
			return RiskDecision{Approved: false, Reason: fmt.Sprintf("%s: 想定比率 %.1f%% が上限 %.1f%% を超える", req.Symbol, weight.Mul(decimal.NewFromInt(100)).InexactFloat64(), r.config.MaxPositionWeight.Mul(decimal.NewFromInt(100)).InexactFloat64())}
		}
	}

	// 10. 総エクスポージャ上限（買いのみ）
	//
	// 1銘柄ごとの比率（9）を守っても、上限いっぱいの銘柄を並べれば
	// 全額が株になる。暴落時に現金が無い状態を避けるため、建玉合計にも
	// 蓋をする。判定は「約定分＋発注中の全銘柄＋今回」で見る。板に残る
	// 買い注文を数えないと、全部約定した瞬間に上限を突破する。
	// 上限が 0（＝未設定の構造体リテラル）のときは判定しない。設定ファイル
	// 経由なら既定 0.90 が入るので、0 は「書き忘れ」であって「全建玉禁止」
	// ではない。
	if req.Side == domain.SideBuy && ctx.Equity.GreaterThan(decimal.Zero) &&
		r.config.MaxGrossExposure.GreaterThan(decimal.Zero) {
		gross := decimal.Zero
		for _, pos := range ctx.Positions {
			gross = gross.Add(pos.MarketValue())
		}
		for _, pending := range ctx.PendingValue {
			gross = gross.Add(pending)
		}
		gross = gross.Add(r.notional(req, ctx))
		ratio := gross.Div(ctx.Equity)

		if ratio.GreaterThan(r.config.MaxGrossExposure) {
			return RiskDecision{Approved: false, Reason: fmt.Sprintf(
				"%s: 建玉合計の想定比率 %.1f%% が総エクスポージャ上限 %.1f%% を超える",
				req.Symbol,
				ratio.Mul(decimal.NewFromInt(100)).InexactFloat64(),
				r.config.MaxGrossExposure.Mul(decimal.NewFromInt(100)).InexactFloat64())}
		}
	}

	// 11. 見積り乖離チェック
	if preview != nil && req.LimitPrice != nil {
		expected := req.LimitPrice.Mul(req.Quantity).Round(0)
		diff := preview.EstimatedCost.Sub(expected).Abs()
		if expected.GreaterThan(decimal.Zero) {
			ratio := diff.Div(expected)
			if ratio.GreaterThan(r.config.MaxPreviewDeviation) {
				return RiskDecision{Approved: false, Reason: fmt.Sprintf("%s: ブローカー見積り %s と自前計算 %s の乖離率 %.2f%% が上限 %.2f%% を超える", req.Symbol, preview.EstimatedCost, expected, ratio.Mul(decimal.NewFromInt(100)).InexactFloat64(), r.config.MaxPreviewDeviation.Mul(decimal.NewFromInt(100)).InexactFloat64())}
			}
		}
	}

	return RiskDecision{Approved: true, Reason: "全リスク項目を通過"}
}

func (r *RiskManager) notional(req domain.OrderRequest, ctx RiskContext) decimal.Decimal {
	if req.LimitPrice != nil {
		return req.LimitPrice.Mul(req.Quantity).Round(0)
	}
	if base, ok := ctx.BasePrices[req.Symbol]; ok {
		return base.Mul(req.Quantity).Round(0)
	}
	return decimal.Zero
}
