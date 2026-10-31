# pull.sh / deploy.sh 共通の定義。source して使う。
#
# サーバごとの実効ツリー = infra/<role>/ に infra/hosts/<host>/ を重ねたもの。
# 管理対象はこのツリーにあるファイルだけ（それ以外の /etc には触らない）。

INFRA_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SSH_KEY=~/.ssh/id_ed25519_github
SSH="ssh -i $SSH_KEY -o ConnectTimeout=10"
ALL_HOSTS="asia1 europe1 america1 asia2"

host_addr() {
  case "$1" in
    asia1) echo isucon@13.159.52.104 ;;
    europe1) echo isucon@52.199.206.161 ;;
    america1) echo isucon@52.197.40.86 ;;
    asia2) echo isucon@3.115.193.33 ;;
    *) echo "unknown host: $1" >&2; return 1 ;;
  esac
}

host_role() {
  case "$1" in
    asia1 | europe1 | america1) echo app ;;
    asia2) echo db ;;
    *) echo "unknown host: $1" >&2; return 1 ;;
  esac
}

# ホストの管理対象ファイルを / からの相対パスで列挙する。
tracked_paths() {
  local role
  role=$(host_role "$1")
  for d in "$INFRA_DIR/$role" "$INFRA_DIR/hosts/$1"; do
    [ -d "$d" ] && (cd "$d" && find . \( -type f -o -type l \) | sed 's|^\./||')
  done | sort -u
}

# 相対パスがこのホストのどのリポジトリ内ファイルに対応するか（overlay 優先）。
repo_path() {
  local host=$1 rel=$2
  if [ -e "$INFRA_DIR/hosts/$host/$rel" ]; then
    echo "$INFRA_DIR/hosts/$host/$rel"
  else
    echo "$INFRA_DIR/$(host_role "$host")/$rel"
  fi
}
