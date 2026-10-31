#!/bin/bash
# 時刻・ブランチによるベンチ制限は廃止。
# 古い PR head の run.sh も FREEZE_CHECK 経由でこのスクリプトを呼ぶため、
# ワークフローと同じ commit から取得する互換エントリーポイントとして残す。
exit 0
