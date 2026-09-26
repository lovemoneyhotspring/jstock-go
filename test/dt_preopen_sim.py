"""寄る前の発注（寄成）の模擬: 気配の誤差を入れて並べ、始値で建てたときのロングの成績。

根拠: vault 20-research/2026-09-jp-daytrade-preopen-order.md
（元の /tmp/dt_preopen_order.py は消えたので、ノートの事前登録から書き直したもの。2026-09-20。
8:59:45 の記録が 20 営業日溜まったら、--slot 085945 で測り直す）

  test/.venv/bin/python test/dt_candidates.py --max-gap 0.03 --out test/out/dt_candidates_wide.parquet
  test/.venv/bin/python test/dt_preopen_sim.py [--slot snap] [--seeds 20] [--err-since 2026-09-11]

形:
  upper    誤差なし。始値のギャップで並べて始値で建てる（上限）
  preopen  見えるギャップ = 始値のギャップ + e。e は板の記録（--slot の時刻）と日足の始値の実測から、
           始値のギャップの帯ごとに独立に引く。見えるギャップで帯 [-1, 0) を絞って順位を付け直し、始値で建てる
  limit    （--limit-on-open のときだけ）preopen と同じ選定で、寄付条件つきの指値を見える値段に置く。
           始値が見える値段以下（e >= 0）の銘柄だけ約定し、約定しない枠は現金のまま。
           **未検証の案。結果を見る前に vault に事前登録を書いてから回すこと**

元の検証との違い（数字は完全には一致しない）:
  - 手仕舞いは日足の引け（元は 15:20 の分足の始値）
  - ロングだけ（ショートの候補表 /tmp/ru_short_cand.parquet も消えていて、ショートは paused 中）
  - 「今の形（9:00:03 のザラ場成行）」は入れていない（ティックの突き合わせが要る。寄成に切り替えたので比較対象でなくなった）
  - 候補は始値のギャップ +3% 未満まで広げてある（元は負だけ）。誤差で負に見える銘柄が候補に入る
学習は 2024-08 以前（本番と同じ設定、test/dt_lgbm_train.py）。12 月は除く。コストは流動性別。
"""

import argparse
import os

import duckdb
import numpy as np
import pandas as pd
from lightgbm import LGBMRegressor, early_stopping, log_evaluation

from dt_candidates import limit_down
from dt_lgbm_train import INNER_VALID_DAYS, KW, VOL_FLOOR, ranked, raw_features
from dt_wf_target import evaluate, liq_cost_bp

CAND = "test/out/dt_candidates_wide.parquet"
BOOK = "state/daytrade/history/book/*.parquet"
BARS = "data/jquants/equities_bars_daily/*.parquet"
TEST_START = "2024-09-02"
HALF = "2025-10-01"
BANDS = [-np.inf, -5.0, -3.0, -1.0, 0.0, 3.0, np.inf]
# 気配の誤差の材料の既定（2026-09-25 ユーザ判断）。"snap" は層にした材料:
#   第一: 8:59:48 以降の値 = 候補は発注時の気配（history/quotes の 8:59 台）、候補の外は寄る前の open が撮る板（slot 085948 以降）
#   補い: 第一に無い銘柄（打ち切りで撮れなかった候補の外など）だけ、同じ日の 8:59:30 の snap（slot 085930）の行
#   （8:59:48 以降の 6 桁の slot には 9/24 だけの 8:59:55 の snap も入る。9/25 の 8:59:42 は第一にも補いにも入れない）
# 8:59:00（"0859"）は 8:59:30 の蓄積が進んだらやめる。2026-09-25 までの結果（slot 0859 で測った数字）を回し直すときは --slot 0859。
# 第一の材料のある日が MIN_SNAP_DAYS に満たなければ error_frame が止める（旧い材料で黙って回さない）
SNAP_SLOT = "snap"
MIN_SNAP_DAYS = 10  # 層の第一の材料が要る最低の日数

ERR_SQL = f"""
WITH b AS (
  SELECT CAST(day AS DATE) d, symbol, TRY_CAST(pPRP AS DOUBLE) pc, TRY_CAST(pQAP AS DOUBLE) ask, TRY_CAST(pQBP AS DOUBLE) bid
  FROM read_parquet('{BOOK}', union_by_name=true) WHERE slot = ? AND CAST(day AS DATE) >= CAST(? AS DATE)),
v AS (
  SELECT d, symbol, pc, CASE WHEN ask > 0 AND bid > 0 THEN (ask + bid) / 2 WHEN ask > 0 THEN ask WHEN bid > 0 THEN bid END vis
  FROM b WHERE pc > 0),
q AS (
  SELECT CAST(Date AS DATE) d, CAST(Code AS VARCHAR) code, TRY_CAST(O AS DOUBLE) op
  FROM read_parquet('{BARS}', union_by_name=true) WHERE CAST(Date AS DATE) >= CAST(? AS DATE))
SELECT v.d, v.symbol, (q.op / v.pc - 1) * 100 g, (v.vis / v.pc - 1) * 100 - (q.op / v.pc - 1) * 100 e
FROM v JOIN q ON q.d = v.d AND q.code = v.symbol || '0' WHERE v.vis IS NOT NULL AND q.op > 0
"""


# 発注時の気配（open が寄る前の回に自分で取った値。history/quotes の 8:59 台）。slot = "order" で使う。
# snap（板の記録）より発注の判断に使った値そのものに近い（2026-09-24 の 1 日で誤差の中央値 0.49% 対 snap 0859 の 0.71%）。
# 寄る前の発注は 2026-09-19 から（最初の取引日は 9/24）
QUOTES = "state/daytrade/history/quotes/*.parquet"
ORDER_ERR_SQL = f"""
WITH v AS (
  SELECT CAST(day AS DATE) d, symbol, prev_close pc, price vis
  FROM read_parquet('{QUOTES}', union_by_name=true)
  WHERE strftime(quote_at AT TIME ZONE 'Asia/Tokyo', '%H:%M') = '08:59' AND price > 0 AND prev_close > 0
    AND CAST(day AS DATE) >= CAST(? AS DATE)),
q AS (
  SELECT CAST(Date AS DATE) d, CAST(Code AS VARCHAR) code, TRY_CAST(O AS DOUBLE) op
  FROM read_parquet('{BARS}', union_by_name=true) WHERE CAST(Date AS DATE) >= CAST(? AS DATE))
SELECT v.d, v.symbol, (q.op / v.pc - 1) * 100 g, (v.vis / v.pc - 1) * 100 - (q.op / v.pc - 1) * 100 e
FROM v JOIN q ON q.d = v.d AND q.code = v.symbol || '0' WHERE q.op > 0
"""


# 寄り直前の全銘柄の板（slot "presnap"）: 9/25 の独立の snap（085942）と、2026-09-28 からは寄る前の open が撮る
# 候補の外の板（slot は open が撮り始めた時刻 HHMMSS。例 085949）。どちらも 8:59:40 以降の 6 桁の slot（085930 の
# snap は入れない）。9/25 より前にあった 6 桁の slot（085945・085955）は since で落とす
PRESNAP_ERR_SQL = ERR_SQL.replace("WHERE slot = ? AND", "WHERE regexp_matches(slot, '^0859[45][0-9]$') AND ? = ? AND")


SNAP_LAYERED_SQL = f"""
WITH o AS ({ORDER_ERR_SQL.replace("SELECT v.d, v.symbol,", "SELECT v.d, v.symbol, 1 AS pri, 'order' AS src,")}),
p AS ({ERR_SQL.replace("WHERE slot = ? AND", "WHERE regexp_matches(slot, '^0859[45][0-9]$') AND slot >= '085948' AND").replace("SELECT v.d, v.symbol,", "SELECT v.d, v.symbol, 2 AS pri, 'presnap' AS src,")}),
s AS ({ERR_SQL.replace("WHERE slot = ? AND", "WHERE slot = '085930' AND").replace("SELECT v.d, v.symbol,", "SELECT v.d, v.symbol, 3 AS pri, '085930' AS src,")}),
u AS (SELECT * FROM o UNION ALL SELECT * FROM p UNION ALL SELECT * FROM s)
SELECT d, symbol, g, e, src FROM u
QUALIFY row_number() OVER (PARTITION BY d, symbol ORDER BY pri) = 1
"""


def error_frame(slot, since):
    """誤差の実測の行（d・symbol・始値のギャップ g・誤差 e、%pt）。slot = "snap" なら層にした既定の材料（上の SNAP_SLOT。
    列 src に出どころ）、"order" なら発注時の気配、"presnap" なら寄り直前の全銘柄の板、ほかは板の記録の時刻。
    環境変数 DT_ERR_UNTIL（YYYY-MM-DD）があればその日までに絞る。記録は毎日増えるので、比べる実行どうしで
    誤差の日数を揃えるために使う（2026-09-25、実行の途中で 7 日 → 8 日に増えて数字が動いた）。
    既定の材料（SNAP_SLOT）を渡された実行は、環境変数 DT_ERR_SLOT があればそれに替える。--slot を持たないスクリプトで
    2026-09-25 までの結果（slot 0859）を再現するため（test/dt_remeasure_*.sh が DT_ERR_SLOT=0859 を付ける）。"""
    if slot == SNAP_SLOT and os.environ.get("DT_ERR_SLOT"):
        slot = os.environ["DT_ERR_SLOT"]
    if slot == "order":
        err = duckdb.sql(ORDER_ERR_SQL, params=[since, since]).df()
    elif slot == "presnap":
        err = duckdb.sql(PRESNAP_ERR_SQL, params=[1, 1, since, since]).df()
    elif slot == "snap":
        err = duckdb.sql(SNAP_LAYERED_SQL, params=[since, since, since, since, since, since]).df()
    else:
        err = duckdb.sql(ERR_SQL, params=[slot, since, since]).df()
    until = os.environ.get("DT_ERR_UNTIL")
    if until:
        err = err[pd.to_datetime(err["d"]) <= pd.Timestamp(until)]
    if slot == "snap":
        first = err.loc[err["src"] != "085930", "d"].nunique()   # 最終日で絞った後に数える
        if first < MIN_SNAP_DAYS:
            raise SystemExit(f"8:59:48 以降の材料のある日が {first} 日で {MIN_SNAP_DAYS} 日に満たない。"
                             f"2026-09-25 までの材料で回すなら --slot 0859")
    return err


ERR_MODES = ("iid", "day", "iid_boot", "day_boot", "model", "model_boot")
# model の残差をまとめる帯の組。行の少ない深い帯（0〜1）だけまとめ、ほかは帯ごと。
# 2026-09-26 の v1（2〜4 をまとめた）は、幅の狭い帯 3 の裾の重い残差を帯 2 に掛けて裾が膨らみ、当てはまりの基準を欠いた
RESID_GROUPS = [(0, 1), (2,), (3,), (4,), (5,)]
MIN_ERR_DAYS = 10   # これより少ない日数の誤差で出した数字は、判定に使わない（2026-09-25、vault 2026-09-jp-daytrade-sim-parity）


class ErrorPools(list):
    """帯ごとの誤差の実測。list としては従来どおり帯ごとの配列（pools[b]）。引き方 mode は draw_errors が見る。
      iid       帯ごとに銘柄ごと独立に引く（従来）
      day       過去の日ごとに実測の 1 日を当て、その日の同じ帯の行から引く（同じ日の銘柄の誤差の相関を残す）
      iid_boot  シードごとに実測の日を復元抽出し直してから iid（誤差の分布が数日分しかない不確かさをシード間に出す）
      day_boot  同じく復元抽出してから day
      model     誤差を「帯の中心 + 日の中心のずれ + 幅 × exp(日の幅のゆれ) × 標準化残差」に分けて引く（fit_error_model）。
                日の揺れは帯ごとの正規分布（標本の揺れを引いた分散）、残差は帯の組ごとに全日をまとめた実測
      model_boot  同じく、シードごとに帯の中心・幅・日の揺れの大きさを推定の不確かさ（日数 n）から引き直してから model"""

    def __init__(self, err, mode):
        super().__init__(err.loc[err["band"] == b, "e"].values for b in range(len(BANDS) - 1))
        self.err, self.mode = err, mode
        self.days = np.array(sorted(err["d"].unique()))
        self.model = fit_error_model(err) if mode.startswith("model") else None


def fit_error_model(err):
    """帯ごとの日の中央値・log MAD（×1.4826）から、中心 mu・幅 s・日の中心のずれの sd omega・日の幅のゆれの sd tau を推定する。
    omega・tau は日ごとの値の分散から標本の揺れ（中央値 1.2533·MAD/√n、log MAD 1.1/√n）を引いた分（負なら 0）。
    残差は (e − その日その帯の中央値) ÷ その日その帯の MAD を帯の組（RESID_GROUPS）でまとめる。"""
    B = len(BANDS) - 1
    mad = lambda x: (x - x.median()).abs().median() * 1.4826  # noqa: E731
    t = err.groupby(["d", "band"])["e"].agg(n="size", med="median", mad=mad)
    t = t[t["mad"] > 0]
    t["lmad"] = np.log(t["mad"])
    m = {k: np.zeros(B) for k in ("mu", "s", "omega", "tau", "vmed", "vlmad")}
    for b in range(B):
        x = t.xs(b, level="band") if b in t.index.get_level_values("band") else t.iloc[:0]
        if len(x) < 2:
            continue
        m["mu"][b], m["s"][b] = x["med"].mean(), x["lmad"].mean()
        m["vmed"][b], m["vlmad"][b] = x["med"].var(), x["lmad"].var()
        m["omega"][b] = np.sqrt(max(0.0, m["vmed"][b] - np.mean((1.2533 * x["mad"]) ** 2 / x["n"])))
        m["tau"][b] = np.sqrt(max(0.0, m["vlmad"][b] - np.mean(1.1 ** 2 / x["n"])))
    key = pd.MultiIndex.from_arrays([err["d"], err["band"]])
    r = (err["e"].values - t["med"].reindex(key).values) / t["mad"].reindex(key).values
    ok = np.isfinite(r)
    m["resid"] = [r[ok & err["band"].isin(grp).values] for grp in RESID_GROUPS]
    m["group"] = np.zeros(B, dtype=int)
    for k, grp in enumerate(RESID_GROUPS):
        m["group"][list(grp)] = k
    m["n_days"] = err["d"].nunique()
    return m


def draw_model(m, te, band, rng, boot, rep=0):
    """fit_error_model の形で誤差を引く。boot なら帯の中心・幅・日の揺れの大きさを推定の不確かさから引き直す。
    rep ≥ 1 なら推定の引き直しの後の乱数を [seed, rep] に替える（seed は rng の元の値を引けないので呼び手の rng から派生させる）"""
    B, n = len(BANDS) - 1, m["n_days"]
    mu, s, omega, tau = m["mu"].copy(), m["s"].copy(), m["omega"].copy(), m["tau"].copy()
    if boot:
        mu += rng.normal(0, np.sqrt(m["vmed"] / n))
        s += rng.normal(0, np.sqrt(m["vlmad"] / n))
        omega *= np.sqrt(rng.chisquare(n - 1, size=B) / (n - 1))
        tau *= np.sqrt(rng.chisquare(n - 1, size=B) / (n - 1))
    if rep:
        rng = np.random.default_rng([int(rng.integers(2**31)), rep])
    di, _ = pd.factorize(te["d"].values)
    nd = di.max() + 1
    delta, v = rng.normal(0, omega, size=(nd, B)), rng.normal(0, tau, size=(nd, B))
    r = np.empty(len(te))
    for k, pool in enumerate(m["resid"]):
        sel = m["group"][band] == k
        if sel.any():
            r[sel] = rng.choice(pool, size=int(sel.sum()))
    return mu[band] + delta[di, band] + np.exp(s[band] + v[di, band]) * r


def error_pools(slot, since, mode=None):
    """帯ごとの誤差 e（見えるギャップ − 始値のギャップ、%pt）の実測。mode を渡さなければ環境変数 DT_ERR_MODE（既定 iid）。"""
    mode = mode or os.environ.get("DT_ERR_MODE", "iid")
    if mode not in ERR_MODES:
        raise ValueError(f"DT_ERR_MODE は {ERR_MODES} のどれか: {mode}")
    err = error_frame(slot, since)
    err["band"] = np.digitize(err["g"], BANDS[1:-1], right=True)
    pools = ErrorPools(err, mode)
    shown = os.environ["DT_ERR_SLOT"] if slot == SNAP_SLOT and os.environ.get("DT_ERR_SLOT") else slot
    print(f"誤差の実測: slot {shown}、{len(pools.days)} 日、引き方 {mode}、帯ごとの行数 {[len(p) for p in pools]}、"
          f"絶対誤差の中央値 {[round(float(np.median(np.abs(p))), 2) if len(p) else None for p in pools]}")
    if len(pools.days) < MIN_ERR_DAYS:
        print(f"**注意: 誤差の実測が {len(pools.days)} 日しかない（{MIN_ERR_DAYS} 日未満）。誤差ありの形の数字は判定に使わない**")
    return pools


def draw_errors(pools, te, seed, rep=0):
    """seed の乱数で、候補 te（gap は小数、d は日付）の気配の誤差 e（%pt）を引く。
    iid（従来の list も）は各スクリプトにあったループと同じ乱数の流れ（過去の数字を再現する）。
    rep ≥ 1 は入れ子の引き直し: *_boot の材料の復元抽出（model_boot は推定の引き直し）は seed のまま同じにし、
    その後の誤差の引きだけ別の乱数にする（tstat_nested が乱数の揺れと材料の不確かさを分けるため）。rep 0 は従来と同じ流れ"""
    band = np.digitize(te["gap"].values * 100, BANDS[1:-1], right=True)
    rng = np.random.default_rng(seed)
    if rep and not getattr(pools, "mode", "iid").endswith("_boot"):
        rng = np.random.default_rng([seed, rep])   # 抽出の無い引き方は全部を別の乱数に
    e = np.zeros(len(te))
    mode = getattr(pools, "mode", "iid")
    if mode == "iid":
        for b, pool in enumerate(pools):
            if (band == b).any():
                e[band == b] = rng.choice(pool, size=int((band == b).sum()))
        return e
    if mode.startswith("model"):
        return draw_model(pools.model, te, band, rng, mode == "model_boot", rep)
    err = pools.err
    days = pools.days
    if mode.endswith("_boot"):
        days = rng.choice(days, size=len(days), replace=True)
        if rep:
            rng = np.random.default_rng([seed, rep])
    rows = pd.concat([err[err["d"] == d] for d in days], ignore_index=True)  # 復元抽出の重複は行の重複として残す
    if mode == "iid_boot":
        for b in range(len(BANDS) - 1):
            pool = rows.loc[rows["band"] == b, "e"].values
            if (band == b).any() and len(pool):
                e[band == b] = rng.choice(pool, size=int((band == b).sum()))
        return e
    # day / day_boot: 過去の日ごとに実測の 1 日を当てる
    hist = pd.Index(np.sort(te["d"].unique()))
    assign = pd.Series(rng.choice(days, size=len(hist)), index=hist)
    errday = assign.reindex(te["d"].values).values
    by = {k: x["e"].values for k, x in err.groupby(["d", "band"])}
    for j in np.unique(errday):
        for b in np.unique(band):
            m = (errday == j) & (band == b)
            if not m.any():
                continue
            pool = by.get((j, b))
            if pool is None or len(pool) == 0:   # その日のその帯に行が無ければ、選んだ日全体の同じ帯から
                pool = rows.loc[rows["band"] == b, "e"].values
            e[m] = rng.choice(pool, size=int(m.sum()))
    return e


def train(df):
    """2024-08 以前の「始値のギャップが負」の候補で学習する（本番の学習と同じ設定）。"""
    tr = df[(df["d"] < TEST_START) & (df["gap"] < 0)].copy()
    tr = with_rule_rank(tr)
    X = ranked(raw_features(tr), tr["d"]).values
    y = tr.groupby("d")["y_raw"].rank(pct=True).values
    days = np.array(sorted(tr["d"].unique()))
    core = (tr["d"] < days[len(days) - INNER_VALID_DAYS]).values
    m = LGBMRegressor(n_estimators=2000, **KW)
    m.fit(X[core], y[core], eval_set=[(X[~core], y[~core])], eval_metric="l2",
          callbacks=[early_stopping(50, verbose=False), log_evaluation(0)])
    final = LGBMRegressor(n_estimators=m.best_iteration_ or 200, **KW)
    final.fit(X, y)
    print(f"学習 {len(tr):,} 行、木 {final.n_estimators} 本")
    return final


def with_rule_rank(g):
    g = g.copy()
    g["key_sort"] = np.where(g["vol20"].notna(),
                             np.round(g["gap"], 4) / np.maximum(g["vol20"].fillna(VOL_FLOOR), VOL_FLOOR), np.inf)
    g = g.sort_values(["d", "key_sort", "code"], kind="mergesort")
    g["rule_rank"] = g.groupby("d").cumcount() + 1
    return g


def seen(te, e_pct):
    """見えるギャップで候補を作り直す。gap・price・順位は見える値、y_raw と gap_true は実際の値。"""
    g = te.copy()
    g["gap_true"] = g["gap"]
    g["gap"] = g["gap"] + e_pct / 100
    g["price"] = g["prev_close"] * (1 + g["gap"])
    g = g[(g["gap"] >= -1.0) & (g["gap"] < 0.0) & (g["price"] > limit_down(g["prev_close"].values))]
    return with_rule_rank(g)


def run(model, g, form, ranker, seed, limit_on_open):
    score = model.predict(ranked(raw_features(g), g["d"]).values) if ranker == "lgbm" else -g["key_sort"].values
    p = evaluate(g, score, f"{form}/{ranker}", seed).merge(g[["d", "code", "gap_true"]], on=["d", "code"])
    p["filled"] = (p["gap_true"] <= p["gap"]) if limit_on_open else True
    return p


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--slot", default=SNAP_SLOT)
    ap.add_argument("--seeds", type=int, default=20)
    ap.add_argument("--err-since", default="2026-09-11")
    ap.add_argument("--limit-on-open", action="store_true")
    ap.add_argument("--cand", default=CAND, help="候補表。元の検証の再現には負のギャップだけの test/out/dt_candidates.parquet")
    a = ap.parse_args()

    pools = error_pools(a.slot, a.err_since)
    df = pd.read_parquet(a.cand)
    df["price"] = df["o"]
    df["sector"] = df["sector"].fillna("")
    model = train(df)
    te = df[(df["d"] >= TEST_START) & (df["d"].dt.month != 12)].copy()
    band = np.digitize(te["gap"].values * 100, BANDS[1:-1], right=True)
    # 候補に出てくる帯だけ見る（候補表を +3% で切っていれば最上位の帯は引かない）
    if empty := [int(b) for b in np.unique(band) if len(pools[b]) == 0]:
        raise SystemExit(f"誤差の実測が 1 行も無い帯があります: {empty}（記録の日数が足りない）")

    picks = []
    exact = seen(te, np.zeros(len(te)))
    for ranker in ("lgbm", "gap_vol"):
        picks.append(run(model, exact, "upper", ranker, 0, False))
    for s in range(a.seeds):
        if pools.mode == "iid":   # 元の乱数の流れのまま（過去の数字を再現する）
            rng = np.random.default_rng(s)
            e = np.empty(len(te))
            for b, pool in enumerate(pools):
                e[band == b] = rng.choice(pool, size=int((band == b).sum()))
        else:
            e = draw_errors(pools, te, s)
        g = seen(te, e)
        for ranker in ("lgbm", "gap_vol"):
            picks.append(run(model, g, "preopen", ranker, s, False))
            if a.limit_on_open:
                picks.append(run(model, g, "limit", ranker, s, True))
    p = pd.concat(picks, ignore_index=True)
    p["ret"] = np.where(p["filled"], p["w"] * (p["y_raw"] - liq_cost_bp(p["turnover_med"].values) / 1e4), 0.0)
    tag = os.path.splitext(os.path.basename(a.cand))[0].replace("dt_candidates", "").strip("_") or "narrow"
    p.to_parquet(f"test/out/dt_preopen_sim_{a.slot}_{tag}_picks.parquet", index=False)

    days = pd.DatetimeIndex(sorted(te["d"].unique()))
    daily = p.groupby(["variant", "seed", "d"])["ret"].sum()
    series = {v: daily[v].groupby("d").mean().reindex(days).fillna(0.0) for v in daily.index.get_level_values(0).unique()}
    print(f"\n{days[0]:%Y-%m-%d}〜{days[-1]:%Y-%m-%d}、{len(days)} 日（12 月を除く）、流動性別コスト、bp/日（建てない日は 0）")
    print("| 形 | bp/日 | t | 前半 / 後半 | −5% 以下の割合（実際の始値） | 約定率 |")
    print("|---|---|---|---|---|---|")
    for v, x in series.items():
        q = p[p["variant"] == v]
        t = x.mean() / (x.std(ddof=1) / np.sqrt(len(x)))
        print(f"| {v} | {x.mean() * 1e4:+.2f} | {t:.2f} | {x[x.index < HALF].mean() * 1e4:+.2f} / {x[x.index >= HALF].mean() * 1e4:+.2f} |"
              f" {(q['gap_true'] <= -0.05).mean() * 100:.1f}% | {q['filled'].mean() * 100:.0f}% |")
    for form in [f for f in ("preopen", "limit") if f"{f}/lgbm" in series]:
        d = series[f"{form}/lgbm"] - series[f"{form}/gap_vol"]
        print(f"{form}: lgbm − gap_vol = {d.mean() * 1e4:+.2f} bp/日（t {d.mean() / (d.std(ddof=1) / np.sqrt(len(d))):.2f}）")
    lost = series["upper/lgbm"] - series["preopen/lgbm"]
    print(f"誤差で失うぶん（lgbm、上限 − preopen）= {lost.mean() * 1e4:+.2f} bp/日")


if __name__ == "__main__":
    main()
