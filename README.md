# 虚実ニュースbot

その日にDiscordで話したユーザーの中からランダムに1人を選び、完全フィクションの「虚実ニュース」を1分〜3時間のランダム間隔で投稿するBotです。

投稿例:

```text
【虚実ニュース・速報】
本日未明、@user が自宅で盛大に脱糞したことが判明し、懲役0.7秒、執行猶予48年を言い渡されました。

専門家は「かなりどうでもいい事件です」と分析しています。

※このニュースは完全なフィクションです。実在の人物・事件・処分とは関係ありません。
```

## インストール

```bash
pip install kyozitu-news-bot
```

初回起動時に、使用しているOS/CPUに合うGo製Bot本体をGitHub Releasesから自動取得します。

対応予定: Windows / Linux / macOS の amd64・arm64。Termuxのarm64環境ではLinux arm64版を使用します。

## 起動方法

Pythonから:

```python
from kyozitu_news_bot import run

run("DISCORD_BOT_TOKEN")
```

または環境変数を設定してCLIから:

```bash
export DISCORD_BOT_TOKEN="YOUR_BOT_TOKEN"
kyozitu-news-bot
```

Windows PowerShell:

```powershell
$env:DISCORD_BOT_TOKEN="YOUR_BOT_TOKEN"
kyozitu-news-bot
```

## 動作

- Bot起動中、その日に発言した非Botユーザーをサーバーごとに記録します。
- 最初の発言後、次の虚実ニュース投稿時刻を1分〜3時間の範囲でランダム決定します。
- 時刻になると、その日に発言したユーザーから1人を選んでメンションします。
- 投稿先は選ばれたユーザーが最後に発言したチャンネルです。
- 投稿後、次回時刻をもう一度1分〜3時間からランダム決定します。
- 日付判定は日本時間です。
- 状態はローカルJSONに保存され、Botを再起動しても保持されます。

## Discord Botの権限

最低限、Botに以下を許可してください。

- View Channels
- Send Messages
- Read Message History

このBotはメッセージ本文を判定しないため、Message Content Intentは不要です。

## データ保存場所

OSのユーザー設定ディレクトリ内の `kyozitu-news-bot/state.json` に保存します。
`KYOZITU_DATA_DIR` 環境変数を指定すると保存先を変更できます。

## 注意

虚実ニュースbotが生成するニュースはすべてネタ・フィクションです。実在のユーザーについて現実の犯罪や処分を事実として伝える用途ではありません。

## 開発

Bot本体はGoで実装しています。PyPIパッケージのPython部分は、Goバイナリの取得と起動だけを担当します。
