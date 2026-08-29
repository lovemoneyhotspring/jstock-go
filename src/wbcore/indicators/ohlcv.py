"""テクニカル指標。polars の式（``pl.Expr``）として組む。

なぜ式を返すのか:
    ``df.with_columns(sma(25), rsi(14), atr(14))`` のように合成でき、
    polars が全体をまとめて最適化・並列実行できる。DataFrame を
    受け取って返す作りにすると、この利点が失われる。

Wilder 平滑化について:
    RSI・ATR・ADX は Wilder の平滑化を使う。素朴に ``ewm_mean`` を
    当てると初期値の取り方が TA-Lib や TradingView と食い違い、
    同じ設定なのに数値が合わないという厄介なズレを生む。

    ここでは **最初の period 本の単純平均を種にする** 正式な定義を
    実装しているため、TA-Lib と一致する。:func:`wilder_ema` を参照。
"""

from __future__ import annotations

import polars as pl

__all__ = [
    "adx",
    "atr",
    "bollinger_bands",
    "donchian_high",
    "donchian_low",
    "ema",
    "macd",
    "roc",
    "rsi",
    "sma",
    "true_range",
    "wilder_ema",
]


def _col(column: str | pl.Expr) -> pl.Expr:
    return pl.col(column) if isinstance(column, str) else column


# --------------------------------------------------------------------------
# 移動平均
# --------------------------------------------------------------------------


def sma(period: int, column: str | pl.Expr = "close") -> pl.Expr:
    """単純移動平均。"""
    _check_period(period)
    return _col(column).rolling_mean(window_size=period).alias(f"sma_{period}")


def ema(period: int, column: str | pl.Expr = "close") -> pl.Expr:
    """指数移動平均（α = 2/(period+1)）。

    最初の period 本の単純平均を種にするため、TA-Lib と一致する。
    """
    _check_period(period)
    return _seeded_ewm(_col(column), period, alpha=2.0 / (period + 1)).alias(f"ema_{period}")


def wilder_ema(expr: pl.Expr, period: int) -> pl.Expr:
    """Wilder の平滑化（α = 1/period）。

    RSI・ATR・ADX の内部で使う。最初の period 本の単純平均を種とし、
    以降は ``avg[i] = avg[i-1] + (x[i] - avg[i-1]) / period`` で更新する。
    """
    _check_period(period)
    return _seeded_ewm(expr, period, alpha=1.0 / period)


def _seeded_ewm(expr: pl.Expr, period: int, *, alpha: float) -> pl.Expr:
    """単純平均を種にした指数平滑。

    仕組み:
        1. ``rolling_mean(period)`` で種を求める。最初に値が確定する
           位置が、ちょうど平滑化を開始すべき位置になる。
        2. その位置だけ種の値に、それ以前は null に、以降は生値に置き換える。
        3. ``ewm_mean(adjust=False, ignore_nulls=True)`` は先頭の null を
           読み飛ばし、最初の非 null を初期値として採用する。

    結果として Wilder / TA-Lib と同じ漸化式になる。
    """
    seed = expr.rolling_mean(window_size=period)
    is_seed_position = seed.is_not_null() & seed.shift(1).is_null()

    seeded = pl.when(seed.is_null()).then(None).when(is_seed_position).then(seed).otherwise(expr)
    return seeded.ewm_mean(alpha=alpha, adjust=False, ignore_nulls=True)


# --------------------------------------------------------------------------
# モメンタム
# --------------------------------------------------------------------------


def rsi(period: int = 14, column: str | pl.Expr = "close") -> pl.Expr:
    """RSI（相対力指数）。0〜100。

    値上がり幅と値下がり幅をそれぞれ Wilder 平滑化して比を取る。
    下落が一度もない区間では 100 になる（ゼロ除算を避けて明示的に扱う）。
    """
    _check_period(period)
    delta = _col(column).diff()
    # delta の先頭は null。`delta > 0` は null と評価され otherwise に落ちるため、
    # 明示的に null を通さないと 0.0 に化けて種が1本ぶん前倒しになり、
    # 平滑化の初期値が汚れて TA-Lib と値がずれる。
    gain = pl.when(delta.is_null()).then(None).when(delta > 0).then(delta).otherwise(0.0)
    loss = pl.when(delta.is_null()).then(None).when(delta < 0).then(-delta).otherwise(0.0)

    avg_gain = wilder_ema(gain, period)
    avg_loss = wilder_ema(loss, period)

    return (
        pl.when(avg_loss == 0)
        .then(100.0)
        .otherwise(100.0 - 100.0 / (1.0 + avg_gain / avg_loss))
        .alias(f"rsi_{period}")
    )


def roc(period: int = 10, column: str | pl.Expr = "close") -> pl.Expr:
    """変化率（%）。"""
    _check_period(period)
    src = _col(column)
    return ((src / src.shift(period) - 1.0) * 100.0).alias(f"roc_{period}")


def macd(
    fast: int = 12,
    slow: int = 26,
    signal: int = 9,
    column: str | pl.Expr = "close",
) -> list[pl.Expr]:
    """MACD。``macd`` / ``macd_signal`` / ``macd_hist`` の3本を返す。

    ``df.with_columns(macd())`` のようにそのまま展開して使う。
    """
    if fast >= slow:
        raise ValueError(f"fast は slow より小さく: fast={fast}, slow={slow}")
    src = _col(column)

    macd_line = _seeded_ewm(src, fast, alpha=2.0 / (fast + 1)) - _seeded_ewm(
        src, slow, alpha=2.0 / (slow + 1)
    )
    signal_line = _seeded_ewm(macd_line, signal, alpha=2.0 / (signal + 1))

    return [
        macd_line.alias("macd"),
        signal_line.alias("macd_signal"),
        (macd_line - signal_line).alias("macd_hist"),
    ]


# --------------------------------------------------------------------------
# ボラティリティ
# --------------------------------------------------------------------------


def true_range(
    high: str | pl.Expr = "high",
    low: str | pl.Expr = "low",
    close: str | pl.Expr = "close",
) -> pl.Expr:
    """真の変動幅。

    前日終値を挟んだ窓・ギャップを取りこぼさないための指標。
    初日は前日終値が無いので ``high - low`` とする（TA-Lib と同じ）。
    """
    h, low_expr, c = _col(high), _col(low), _col(close)
    prev_close = c.shift(1)

    return (
        pl.when(prev_close.is_null())
        .then(h - low_expr)
        .otherwise(
            pl.max_horizontal(
                h - low_expr,
                (h - prev_close).abs(),
                (low_expr - prev_close).abs(),
            )
        )
        .alias("true_range")
    )


def atr(
    period: int = 14,
    high: str | pl.Expr = "high",
    low: str | pl.Expr = "low",
    close: str | pl.Expr = "close",
) -> pl.Expr:
    """ATR（平均真の変動幅）。

    損切り幅とポジションサイジングの基準に使う。値幅を価格水準に
    依存しない形で測れるので、銘柄をまたいでリスクを揃えられる。
    """
    _check_period(period)
    return wilder_ema(true_range(high, low, close), period).alias(f"atr_{period}")


def bollinger_bands(
    period: int = 20,
    num_std: float = 2.0,
    column: str | pl.Expr = "close",
) -> list[pl.Expr]:
    """ボリンジャーバンド。``bb_mid`` / ``bb_upper`` / ``bb_lower`` を返す。

    標準偏差は母集団標準偏差（ddof=0）。TA-Lib と同じ定義。
    """
    _check_period(period)
    src = _col(column)
    mid = src.rolling_mean(window_size=period)
    std = src.rolling_std(window_size=period, ddof=0)

    return [
        mid.alias("bb_mid"),
        (mid + std * num_std).alias("bb_upper"),
        (mid - std * num_std).alias("bb_lower"),
    ]


# --------------------------------------------------------------------------
# ブレイクアウト
# --------------------------------------------------------------------------


def donchian_high(period: int = 20, high: str | pl.Expr = "high") -> pl.Expr:
    """過去 period 本の最高値（当日を除く）。

    当日を含めるとブレイク判定が常に成立してしまうため、1本ずらす。
    ここを間違えると「必ず勝つ」バックテスト結果が出る典型的な罠になる。
    """
    _check_period(period)
    return _col(high).shift(1).rolling_max(window_size=period).alias(f"donchian_high_{period}")


def donchian_low(period: int = 20, low: str | pl.Expr = "low") -> pl.Expr:
    """過去 period 本の最安値（当日を除く）。"""
    _check_period(period)
    return _col(low).shift(1).rolling_min(window_size=period).alias(f"donchian_low_{period}")


# --------------------------------------------------------------------------
# トレンドの強さ
# --------------------------------------------------------------------------


def adx(
    period: int = 14,
    high: str | pl.Expr = "high",
    low: str | pl.Expr = "low",
    close: str | pl.Expr = "close",
) -> list[pl.Expr]:
    """ADX と ±DI。``adx`` / ``di_plus`` / ``di_minus`` を返す。

    ADX は方向を示さず「トレンドの強さ」だけを示す。逆張り戦略の
    フィルタ（強トレンド時は逆張りしない）に使うと効く。
    """
    _check_period(period)
    h, low_expr = _col(high), _col(low)

    up_move = h - h.shift(1)
    down_move = low_expr.shift(1) - low_expr

    # RSI と同じ理由で、先頭の null は明示的に通す（0.0 に化けさせない）
    is_first = up_move.is_null() | down_move.is_null()
    plus_dm = (
        pl.when(is_first)
        .then(None)
        .when((up_move > down_move) & (up_move > 0))
        .then(up_move)
        .otherwise(0.0)
    )
    minus_dm = (
        pl.when(is_first)
        .then(None)
        .when((down_move > up_move) & (down_move > 0))
        .then(down_move)
        .otherwise(0.0)
    )

    smoothed_tr = wilder_ema(true_range(high, low, close), period)
    di_plus = 100.0 * wilder_ema(plus_dm, period) / smoothed_tr
    di_minus = 100.0 * wilder_ema(minus_dm, period) / smoothed_tr

    di_sum = di_plus + di_minus
    dx = pl.when(di_sum == 0).then(0.0).otherwise(100.0 * (di_plus - di_minus).abs() / di_sum)

    return [
        wilder_ema(dx, period).alias(f"adx_{period}"),
        di_plus.alias("di_plus"),
        di_minus.alias("di_minus"),
    ]


def _check_period(period: int) -> None:
    if period < 1:
        raise ValueError(f"period は 1 以上: {period}")
