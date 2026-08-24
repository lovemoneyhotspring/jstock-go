"""インジケーターのテスト。

polars の式で書いた実装を、**素朴な純 Python の参照実装**と突き合わせる。
同じロジックを別の書き方で二度実装して一致を見るので、polars 特有の
落とし穴（null の扱い、窓のずれ）を検出できる。
"""

from __future__ import annotations

import math

import polars as pl
import pytest

from wbjp.indicators.ohlcv import (
    adx,
    atr,
    bollinger_bands,
    donchian_high,
    donchian_low,
    ema,
    macd,
    roc,
    rsi,
    sma,
    true_range,
    wilder_ema,
)

# Wilder『New Concepts in Technical Trading Systems』の RSI 検証用データ
WILDER_CLOSES = [
    44.34, 44.09, 44.15, 43.61, 44.33, 44.83, 45.10, 45.42, 45.84, 46.08,
    45.89, 46.03, 45.61, 46.28, 46.28, 46.00, 46.03, 46.41, 46.22, 45.64,
]  # fmt: skip


@pytest.fixture
def ohlcv() -> pl.DataFrame:
    """再現性のある擬似的な値動き。"""
    closes = []
    price = 1000.0
    for i in range(120):
        price *= 1 + 0.012 * math.sin(i / 3.7) + 0.004 * math.cos(i / 1.3)
        closes.append(round(price, 2))
    return pl.DataFrame(
        {
            "close": closes,
            "high": [round(c * 1.008, 2) for c in closes],
            "low": [round(c * 0.991, 2) for c in closes],
        }
    )


# --------------------------------------------------------------------------
# 参照実装（純 Python）
# --------------------------------------------------------------------------


def ref_wilder(values: list[float | None], period: int) -> list[float | None]:
    """Wilder 平滑化の参照実装。

    最初の period 個（null を除く）の単純平均を種にし、以降は
    avg = avg + (x - avg) / period で更新する。
    """
    out: list[float | None] = [None] * len(values)
    clean = [(i, v) for i, v in enumerate(values) if v is not None]
    if len(clean) < period:
        return out

    seed_idx = clean[period - 1][0]
    avg = sum(v for _, v in clean[:period]) / period
    out[seed_idx] = avg

    for i, v in clean[period:]:
        avg = avg + (v - avg) / period
        out[i] = avg
    return out


def ref_rsi(closes: list[float], period: int) -> list[float | None]:
    deltas: list[float | None] = [None] + [closes[i] - closes[i - 1] for i in range(1, len(closes))]
    gains = [None if d is None else max(d, 0.0) for d in deltas]
    losses = [None if d is None else max(-d, 0.0) for d in deltas]

    avg_gain = ref_wilder(gains, period)
    avg_loss = ref_wilder(losses, period)

    out: list[float | None] = []
    for g, loss in zip(avg_gain, avg_loss, strict=True):
        if g is None or loss is None:
            out.append(None)
        elif loss == 0:
            out.append(100.0)
        else:
            out.append(100.0 - 100.0 / (1.0 + g / loss))
    return out


def ref_true_range(highs: list[float], lows: list[float], closes: list[float]) -> list[float]:
    out = [highs[0] - lows[0]]
    for i in range(1, len(closes)):
        prev = closes[i - 1]
        out.append(max(highs[i] - lows[i], abs(highs[i] - prev), abs(lows[i] - prev)))
    return out


def _close(actual: list[float | None], expected: list[float | None]) -> None:
    assert len(actual) == len(expected)
    for i, (a, e) in enumerate(zip(actual, expected, strict=True)):
        if e is None:
            assert a is None, f"index {i}: {a} が None であるべき"
        else:
            assert a is not None, f"index {i}: None ではなく {e} であるべき"
            assert a == pytest.approx(e, rel=1e-9, abs=1e-9), f"index {i}"


# --------------------------------------------------------------------------
# RSI
# --------------------------------------------------------------------------


def test_rsi_matches_hand_calculation() -> None:
    """Wilder の定義から手計算した値と一致すること。

    値上がり幅の合計 3.34、値下がり幅の合計 1.40、期間14。
        avg_gain = 3.34/14 = 0.2385714…
        avg_loss = 1.40/14 = 0.1
        RS  = 2.3857142…
        RSI = 100 - 100/(1+RS) = 70.4641…
    """
    df = pl.DataFrame({"close": WILDER_CLOSES})
    got = df.with_columns(rsi(14))["rsi_14"].to_list()
    assert got[14] == pytest.approx(70.46411, abs=1e-4)


def test_rsi_first_value_lands_on_the_correct_bar() -> None:
    """期間14なら、14本ぶんの変化が揃う index=14 が最初の値。

    ここが1本ずれると平滑化の種が汚れ、以降ずっと値が狂う。
    """
    df = pl.DataFrame({"close": WILDER_CLOSES})
    got = df.with_columns(rsi(14))["rsi_14"].to_list()
    assert all(v is None for v in got[:14])
    assert got[14] is not None


def test_rsi_matches_reference_implementation(ohlcv: pl.DataFrame) -> None:
    closes = ohlcv["close"].to_list()
    got = ohlcv.with_columns(rsi(14))["rsi_14"].to_list()
    _close(got, ref_rsi(closes, 14))


def test_rsi_is_bounded(ohlcv: pl.DataFrame) -> None:
    for value in ohlcv.with_columns(rsi(14))["rsi_14"].to_list():
        if value is not None:
            assert 0.0 <= value <= 100.0


def test_rsi_is_100_when_price_only_rises() -> None:
    """下落が無い区間ではゼロ除算になるため、100 で明示的に返す。"""
    df = pl.DataFrame({"close": [float(100 + i) for i in range(30)]})
    assert df.with_columns(rsi(14))["rsi_14"].to_list()[-1] == 100.0


def test_rsi_is_zero_when_price_only_falls() -> None:
    df = pl.DataFrame({"close": [float(100 - i) for i in range(30)]})
    assert df.with_columns(rsi(14))["rsi_14"].to_list()[-1] == pytest.approx(0.0)


# --------------------------------------------------------------------------
# Wilder 平滑化
# --------------------------------------------------------------------------


def test_wilder_ema_matches_reference(ohlcv: pl.DataFrame) -> None:
    got = ohlcv.select(wilder_ema(pl.col("close"), 14).alias("w"))["w"].to_list()
    _close(got, ref_wilder(ohlcv["close"].to_list(), 14))


def test_wilder_ema_seed_is_simple_average() -> None:
    """種が単純平均であること（TA-Lib 互換の要）。"""
    values = [float(v) for v in range(1, 21)]
    got = pl.DataFrame({"x": values}).select(wilder_ema(pl.col("x"), 5).alias("w"))["w"].to_list()
    assert got[:4] == [None] * 4
    assert got[4] == pytest.approx(sum(values[:5]) / 5)  # = 3.0


# --------------------------------------------------------------------------
# ATR / True Range
# --------------------------------------------------------------------------


def test_true_range_matches_reference(ohlcv: pl.DataFrame) -> None:
    got = ohlcv.with_columns(true_range())["true_range"].to_list()
    _close(got, list(ref_true_range(*[ohlcv[c].to_list() for c in ("high", "low", "close")])))


def test_true_range_first_bar_uses_high_minus_low() -> None:
    """初日は前日終値が無いので high - low（TA-Lib と同じ扱い）。"""
    df = pl.DataFrame({"high": [110.0], "low": [100.0], "close": [105.0]})
    assert df.with_columns(true_range())["true_range"].to_list() == [10.0]


def test_true_range_captures_gap_up() -> None:
    """窓を開けて上放れた日は、値幅ではなくギャップを含めて測る。"""
    df = pl.DataFrame({"high": [100.0, 130.0], "low": [95.0, 125.0], "close": [98.0, 128.0]})
    # 2日目: high-low=5 だが、前日終値98からは130-98=32
    assert df.with_columns(true_range())["true_range"].to_list()[1] == pytest.approx(32.0)


def test_atr_matches_reference(ohlcv: pl.DataFrame) -> None:
    highs, lows, closes = (ohlcv[c].to_list() for c in ("high", "low", "close"))
    expected = ref_wilder(list(ref_true_range(highs, lows, closes)), 14)
    got = ohlcv.with_columns(atr(14))["atr_14"].to_list()
    _close(got, expected)


def test_atr_is_positive(ohlcv: pl.DataFrame) -> None:
    for value in ohlcv.with_columns(atr(14))["atr_14"].to_list():
        if value is not None:
            assert value > 0


# --------------------------------------------------------------------------
# 移動平均
# --------------------------------------------------------------------------


def test_sma_matches_reference(ohlcv: pl.DataFrame) -> None:
    closes = ohlcv["close"].to_list()
    expected = [None] * 24 + [sum(closes[i - 24 : i + 1]) / 25 for i in range(24, len(closes))]
    _close(ohlcv.with_columns(sma(25))["sma_25"].to_list(), expected)


def test_ema_seed_is_simple_average() -> None:
    values = [float(v) for v in range(1, 21)]
    got = pl.DataFrame({"close": values}).with_columns(ema(5))["ema_5"].to_list()
    assert got[:4] == [None] * 4
    assert got[4] == pytest.approx(3.0)
    # 以降は alpha = 2/(5+1) = 1/3
    assert got[5] == pytest.approx(3.0 + (6.0 - 3.0) / 3)


def test_moving_averages_have_correct_warmup(ohlcv: pl.DataFrame) -> None:
    out = ohlcv.with_columns(sma(25), ema(25))
    for column in ("sma_25", "ema_25"):
        values = out[column].to_list()
        assert all(v is None for v in values[:24]), column
        assert values[24] is not None, column


# --------------------------------------------------------------------------
# MACD / ボリンジャーバンド
# --------------------------------------------------------------------------


def test_macd_histogram_is_difference(ohlcv: pl.DataFrame) -> None:
    out = ohlcv.with_columns(macd())
    for line, signal, hist in zip(out["macd"], out["macd_signal"], out["macd_hist"], strict=True):
        if None in (line, signal, hist):
            continue
        assert hist == pytest.approx(line - signal)


def test_macd_rejects_fast_not_less_than_slow() -> None:
    with pytest.raises(ValueError, match="fast は slow より小さく"):
        macd(fast=26, slow=12)


def test_bollinger_bands_are_symmetric(ohlcv: pl.DataFrame) -> None:
    out = ohlcv.with_columns(bollinger_bands(20, 2.0))
    for mid, upper, lower in zip(out["bb_mid"], out["bb_upper"], out["bb_lower"], strict=True):
        if None in (mid, upper, lower):
            continue
        assert upper - mid == pytest.approx(mid - lower)
        assert upper >= mid >= lower


def test_bollinger_bands_collapse_on_flat_price() -> None:
    """値動きが無ければ標準偏差 0 でバンドは1本に潰れる。"""
    df = pl.DataFrame({"close": [100.0] * 30})
    out = df.with_columns(bollinger_bands(20))
    assert out["bb_upper"].to_list()[-1] == pytest.approx(100.0)
    assert out["bb_lower"].to_list()[-1] == pytest.approx(100.0)


# --------------------------------------------------------------------------
# ドンチャン — 先読みバイアスの防止
# --------------------------------------------------------------------------


def test_donchian_excludes_current_bar() -> None:
    """当日を含めるとブレイク判定が常に成立し、必勝の嘘が生まれる。

    ここは先読みバイアスの入り口なので、明示的に固定する。
    """
    highs = [10.0, 11.0, 12.0, 50.0]
    df = pl.DataFrame({"high": highs, "low": [h - 1 for h in highs]})
    got = df.with_columns(donchian_high(3))["donchian_high_3"].to_list()

    # index 3 の値は index 0..2 の最高値（12.0）であり、当日の 50.0 ではない
    assert got[3] == 12.0
    assert got[:3] == [None, None, None]


def test_donchian_low_excludes_current_bar() -> None:
    lows = [10.0, 9.0, 8.0, 1.0]
    df = pl.DataFrame({"low": lows, "high": [x + 1 for x in lows]})
    got = df.with_columns(donchian_low(3))["donchian_low_3"].to_list()
    assert got[3] == 8.0


def test_donchian_breakout_is_detectable(ohlcv: pl.DataFrame) -> None:
    """当日高値がドンチャン上限を超えた日が実際に検出できること。"""
    out = ohlcv.with_columns(donchian_high(20)).filter(
        pl.col("donchian_high_20").is_not_null() & (pl.col("high") > pl.col("donchian_high_20"))
    )
    assert out.height > 0


# --------------------------------------------------------------------------
# ADX / ROC
# --------------------------------------------------------------------------


def test_adx_is_bounded(ohlcv: pl.DataFrame) -> None:
    out = ohlcv.with_columns(adx(14))
    for column in ("adx_14", "di_plus", "di_minus"):
        for value in out[column].to_list():
            if value is not None:
                assert 0.0 <= value <= 100.0, column


def test_adx_is_high_in_a_strong_trend() -> None:
    """一方向に伸び続ける相場では ADX が大きくなる。"""
    closes = [100.0 * (1.02**i) for i in range(80)]
    df = pl.DataFrame(
        {"close": closes, "high": [c * 1.005 for c in closes], "low": [c * 0.995 for c in closes]}
    )
    assert df.with_columns(adx(14))["adx_14"].to_list()[-1] > 50.0


def test_roc_computes_percentage_change() -> None:
    df = pl.DataFrame({"close": [100.0, 110.0, 121.0]})
    got = df.with_columns(roc(1))["roc_1"].to_list()
    assert got[0] is None
    assert got[1] == pytest.approx(10.0)
    assert got[2] == pytest.approx(10.0)


# --------------------------------------------------------------------------
# 入力チェック
# --------------------------------------------------------------------------


@pytest.mark.parametrize("factory", [sma, ema, rsi, atr, donchian_high, donchian_low, roc])
def test_period_must_be_positive(factory) -> None:  # type: ignore[no-untyped-def]
    with pytest.raises(ValueError, match="period"):
        factory(0)


def test_indicators_compose_in_a_single_pass(ohlcv: pl.DataFrame) -> None:
    """複数の指標を一度の with_columns でまとめて計算できること。"""
    out = ohlcv.with_columns(
        sma(25), ema(25), rsi(14), atr(14), donchian_high(20), *macd(), *bollinger_bands()
    )
    expected = {
        "sma_25", "ema_25", "rsi_14", "atr_14", "donchian_high_20",
        "macd", "macd_signal", "macd_hist", "bb_mid", "bb_upper", "bb_lower",
    }  # fmt: skip
    assert expected <= set(out.columns)
    assert out.height == ohlcv.height
