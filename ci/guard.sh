# サーバの状態を変える手元スクリプトの先頭で source する。
# デプロイとベンチは CI（.github/workflows/bench.yml）だけが 1 本ずつ行う。手元から配ると、
# 他の人のベンチの最中にサーバが変わり、どの変更でスコアが動いたか分からなくなる。
# 緊急時（CI が壊れた等）だけ、チームに宣言してから ALLOW_LOCAL_DEPLOY=1 を付けて実行する。
if [ -z "${GITHUB_ACTIONS:-}" ] && [ "${ALLOW_LOCAL_DEPLOY:-}" != 1 ]; then
  cat >&2 <<'EOF'
ERROR: デプロイ・スモーク・ベンチは CI からのみ実行する。
  isucon26-container の PR に `/bench [メモ]` とコメントする。
  main を回す: gh workflow run bench.yml -R Harineko0/isucon26-container
  詳細: ci/README.md
EOF
  exit 1
fi
