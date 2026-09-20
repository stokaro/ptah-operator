<p align="center"><img src="docs/site/src/assets/logo.svg" alt="Ptah のマーク。濃色の角丸正方形の上に、空色の二層と琥珀色の冠石が重なる図" width="72" height="72"></p>

<h1 align="center">Ptah Operator</h1>

<p align="center">Ptah（プタハ）Operator は、PostgreSQL と MySQL のスキーマを不変の OCI アーティファクトから収束させる Kubernetes コントロールプレーンです。</p>

<p align="center"><a href="README.md">English</a> · <strong>日本語</strong></p>

<p align="center">
  <a href="https://github.com/stokaro/ptah-operator/actions/workflows/ci.yml?query=branch%3Amaster"><img src="https://img.shields.io/github/actions/workflow/status/stokaro/ptah-operator/ci.yml?branch=master&label=ci&logo=github" alt="master ブランチにおける CI ワークフローの状態。ソースを検証し、レース検出器を実行し、Kubernetes のライフサイクル全体を動かす"></a>
  <a href="https://github.com/stokaro/ptah-operator/actions/workflows/demo.yml?query=branch%3Amaster"><img src="https://img.shields.io/github/actions/workflow/status/stokaro/ptah-operator/demo.yml?branch=master&label=demonstration&logo=github" alt="master ブランチにおける週次デモンストレーションワークフローの状態。実クラスタに対して全シナリオを録り直す"></a>
  <a href="https://github.com/stokaro/ptah-operator/actions/workflows/docs.yml?query=branch%3Amaster"><img src="https://img.shields.io/github/actions/workflow/status/stokaro/ptah-operator/docs.yml?branch=master&label=docs&logo=github" alt="master ブランチにおけるドキュメントワークフローの状態"></a>
  <a href="https://github.com/stokaro/ptah-operator/blob/master/LICENSE"><img src="https://img.shields.io/github/license/stokaro/ptah-operator?label=license&color=blue" alt="MIT と表示されるライセンスバッジ"></a>
  <a href="https://github.com/stokaro/ptah-operator/blob/master/go.mod"><img src="https://img.shields.io/github/go-mod/go-version/stokaro/ptah-operator?label=go&logo=go&logoColor=white" alt="go.mod が宣言する Go のバージョン"></a>
</p>

<p align="center"><a href="https://operator.ptah.run/edge/start/install/">インストール</a> · <a href="https://operator.ptah.run/edge/start/first-schema/">最初のスキーマ</a> · <a href="https://operator.ptah.run/demo/">記録された実行</a> · <a href="https://operator.ptah.run/">ドキュメント</a> · <a href="https://docs.ptah.run/compatibility/operator/">Ptah との互換性</a></p>

Ptah Operator は、PostgreSQL と MySQL のスキーマを不変の OCI アーティファクトから
継続的に収束させる、Kubernetes ネイティブのコントロールプレーンです。データベースに対する
作業は短命で堅牢化された Job の中で実行され、コントローラー自身はデータベースの Secret を
読む権限を持ちません。

`PtahSchema` はデータベースの構造と、宣言が指定する参照行を収束させます。`PtahMigration`
は準備済みのマイグレーション列を実行し、記録された履歴を、選択したアーティファクトに
束縛します。チェックポイントによるブートストラップも含みます。どちらも PostgreSQL と
MySQL で動作します。

API は現在 `v1alpha1` です。エンドツーエンドのマトリクスは緑です。対応するすべての
Kubernetes マイナーバージョン、両方のエンジン、両方のアーティファクト形式が通っています。
これが実装プレビューでなくなるまでに残っているのは、公開されたリリースだけです。ある実行が
何を測定し、どの Ptah ビルドに対してだったかは
[`support/ptah.json`](support/ptah.json) に記録されています。

## 収束モデル

```text
resolve tag to digest -> verify artifact -> observe database -> publish plan
       ^                                                        |
       |                         approval (when required) <------+
       |                                                        |
       +--- verify convergence <- apply exact approved plan <---+
```

コントローラーは各 Job を作成する前にクレームを記録し、`PtahSchema` ごとに同時に動く Job
を最大 1 つに制限し、再起動後はそのクレームを引き継ぎます。プロセスの終了は収束の証明として
扱いません。適用のあとには必ず、読み取り専用の観測をあらためて行います。

主要な安全性の性質:

- OCI タグの解決は一度だけ行い、以降のアーティファクトへのアクセスはすべてダイジェストを
  使います。
- 計画は、正確なバイト列をアーティファクト、対象、観測された状態、ポリシー、ダイジェストで
  固定されたマネージャーイメージ、マネージャーのリビジョンと状態の意味、Ptah のバージョン、
  エグゼキューターイメージ、ランナーイメージ、ランナープロトコルに束縛します。
- 承認は独立した不変リソースであり、認証された admission の識別情報が刻まれます。
- 破壊的な計画は既定で無効です。有効にした場合でも、計画そのものに対する承認が必要です。
- データベースとレジストリの資格情報は互いに隔離され、status、Event、計画リソース、
  コマンド引数のいずれにも置かれません。
- 削除と一時停止がクリーンアップ SQL を実行することはありません。
- 適用の結果が不確かな場合は、再実行ではなく必ず観測に戻ります。

## 何が適用されたかを読む

SQL は不変の ConfigMap にあり、計画がそれをインデックス、サイズ、ダイジェストで束縛
します。status フィールドやログ行には入りません。`kubectl ptah` は、オペレーターと同じ
方法でそれを読み戻します。

```sh
kubectl ptah plan storefront --applied -n application -o sql
```

これは読み取り専用のクライアントで、対応するクライアントプラットフォーム向けに `kubectl`
プラグインとして各リリースで公開されます。インストール方法と必要な名前空間スコープの RBAC
は[計画を読む](https://operator.ptah.run/edge/use/read-a-plan/#install)にあります。

## インストールと最初のスキーマ

ガイドは専用のサイト [operator.ptah.run](https://operator.ptah.run/) にあります。
インストール、チャートが推測を拒む 3 つの値、最初のスキーマの実例、設定、運用、
セキュリティモデル、condition の reason、サポート期間を扱っています。

```sh
cd docs/site && npm ci && npm run build
```

このチェックアウトからサイトをビルドします。

## 実際の動作を見る

[記録された実行](https://operator.ptah.run/demo/)は、オペレーターが実クラスタに対して
動作している間に記録した端末セッションです。スキーマの適用、変更、計画の承認、破壊的変更の
拒否、ドリフトの解消、失敗と復旧が含まれます。各セッションは記録中に検査されます。
セッションが主張する condition は稼働中のオブジェクトから読み取られ、すべての検査が
通った場合にのみ公開されます。

上の `demonstration` バッジは `master` に対する週次の録り直しであり、サイトが再生する
セッションではありません。緑は、`master` からビルドしたクラスタに対してすべてのシナリオが
まだ成り立っていることを意味します。読者が見るのは `demo/recordings/runs.json` に
コミットされた録画で、これは誰かが記録したときにだけ変わります。したがって赤いバッジは、
真でなくなったデモンストレーションを意味します。読者がそれを現状として受け取る前に知って
おく価値があります。

[`demo/`](demo/README.md) に、その作り方と自分で実行する方法があります。

## ドキュメント

利用者向けガイドは [operator.ptah.run](https://operator.ptah.run/) で公開され、その
ページは `docs/site/src/content/docs` にあります。そのうちの 1 ページは、これを運用する
人ではなく、このコードを変更する人のために書かれています。

- [アーキテクチャ](https://operator.ptah.run/edge/reference/architecture/) --
  構成要素、それぞれの置き場所、各要素が守る不変条件。

Ptah 自体、つまりスキーマ形式、OCI アーティファクトのレイアウト、CLI は
[docs.ptah.run](https://docs.ptah.run/edge/) に文書化されています。どの Ptah ビルドが
このオペレーターで検証済みかは
[互換性マトリクス](https://docs.ptah.run/compatibility/operator/)で公開しています。

`PtahMigration` は意図的に `PtahSchema` に統合していません。宣言された行の集合は望ましい
行を記述し、マイグレーション列は状態間の遷移を記述します。そのため両者は OCI 転送、
資格情報の隔離、実行プロトコル、対象の調整を共有しつつ、それぞれの API と状態機械を
保ちます。

## ライセンスとサポート

Ptah Operator は [MIT ライセンス](LICENSE)で公開されています。

質問とバグ報告は
[stokaro/ptah-operator](https://github.com/stokaro/ptah-operator/issues) の issue で
お願いします。Ptah CLI が単独で行うことについては
[stokaro/ptah](https://github.com/stokaro/ptah/issues) にお願いします。
報告を実行可能にする条件と、変更が通すべきものは
[CONTRIBUTING.md](CONTRIBUTING.md) にあります。参加については
[行動規範](CODE_OF_CONDUCT.md)が適用されます。商用のお問い合わせは
`ask@stokaro.com` までお願いします。
