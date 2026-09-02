"""共通基盤。取引プロジェクト（``wbjp`` スイング売買 / ``accum`` 積立）が共有する部品。

ここに置くのは「どの取引方針にも依存しない」ものだけ:

- :mod:`wbcore.domain`      注文・建玉・市場ルールなどのモデル
- :mod:`wbcore.broker`      証券会社への発注（今はペーパーのみ）
- :mod:`wbcore.data`        足データの取得と保存（J-Quants / FRED）
- :mod:`wbcore.indicators`  polars 式の指標
- :mod:`wbcore.credentials` APIキーの解決（キーチェーン・環境変数・.env）
- :mod:`wbcore.settings`    環境変数由来の設定と、実発注の可否判定
- :mod:`wbcore.registry`    名前でクラスを引く登録簿（戦略の登録に使う）
- :mod:`wbcore.logging`     秘密を伏せる構造化ログ

依存の向きは一方通行。``wbcore`` は ``wbjp`` / ``accum`` を import しない。
どの部品も単独で組み合わせられる（ブローカーだけ・データ層だけ、など）。
"""
