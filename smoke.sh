#!/bin/bash
# ルーター経由で主要 API を一通り叩く簡易スモークテスト。最後に initialize で元に戻す。
set -u
R=http://35.73.68.236:8080
IMG=${IMG:-/private/tmp/claude-501/-Users-hari-proj-isucon26/cbfe4815-0498-4588-91b7-438e9a4643b6/scratchpad/test.jpg}
S=$$
j() { curl -s -H 'Content-Type: application/json' "$@"; }
code() { curl -s -o /dev/null -w "%{http_code}" "$@"; }
j -XPOST $R/api/initialize; echo
reg() { j -XPOST $R/api/auth/register -d "{\"username\":\"$1\",\"displayName\":\"t\",\"region\":\"$2\"}"; }
login() { j -XPOST $R/api/auth/login -H "X-Country-Code: $3" -d "{\"username\":\"$1\",\"secretKey\":\"$2\"}" | jq -r .token; }
A=$(reg sm_a_$S europe); B=$(reg sm_b_$S europe); C=$(reg sm_c_$S europe)
TA=$(login sm_a_$S $(echo $A|jq -r .secretKey) GB); TB=$(login sm_b_$S $(echo $B|jq -r .secretKey) GB); TC=$(login sm_c_$S $(echo $C|jq -r .secretKey) GB)
AID=$(echo $A|jq -r .user.id); BID=$(echo $B|jq -r .user.id); CID=$(echo $C|jq -r .user.id)
H="-H X-Country-Code:GB"
RQ=$(j -XPOST $R/api/friend-requests $H -H "Authorization: Bearer $TA" -d "{\"recipientId\":\"$BID\"}" | jq -r .relationId)
echo "accept $(code -XPOST $R/api/friend-requests/$RQ/accept $H -H "Authorization: Bearer $TB")"
RQ=$(j -XPOST $R/api/friend-requests $H -H "Authorization: Bearer $TB" -d "{\"recipientId\":\"$CID\"}" | jq -r .relationId)
echo "accept $(code -XPOST $R/api/friend-requests/$RQ/accept $H -H "Authorization: Bearer $TC")"
SSE=/tmp/smoke_sse_$S; (curl -s -N -m 6 $R/api/notifications $H -H "Authorization: Bearer $TC" > $SSE &) ; sleep 1.5
AT=$(j -XPOST $R/api/admin/auth/login -d '{"username":"admin","password":"password1234"}' | jq -r .token)
j -XPOST $R/api/admin/posting-window -H "Authorization: Bearer $AT" -d '{"region":"europe","localDate":"2026-12-01","onTimeSeconds":60}' >/dev/null
post() { curl -s -XPOST $R/api/posts $H -H "Authorization: Bearer $1" -F image=@$IMG -F caption=c -F visibility=$2 -F location_kind=none | jq -r .id; }
sleep 1; echo "SSE: $(cat $SSE)"
PA=$(post $TA friends_of_friends); PB=$(post $TB friends); PC=$(post $TC public)
echo "posts $PA $PB $PC"
for f in friends friends-of-friends public following; do echo "C feed $f: $(curl -s $R/api/feeds/$f $H -H "Authorization: Bearer $TC" | jq -c '[.items[].author.username]')"; done
echo "C get A post (fof): $(code $R/api/posts/$PA $H -H "Authorization: Bearer $TC")"
echo "C comment A post (fof, not friend): $(code -XPOST $R/api/posts/$PA/comments $H -H "Authorization: Bearer $TC" -H 'Content-Type: application/json' -d '{"body":"hi"}')"
echo "B comment A post: $(j -XPOST $R/api/posts/$PA/comments $H -H "Authorization: Bearer $TB" -d '{"body":"hi"}' | jq -c '[.body,.author.username]')"
echo "C realmoji A post: $(curl -s -XPOST $R/api/posts/$PA/realmojis $H -H "Authorization: Bearer $TC" -F image=@$IMG -F reactionType=like | jq -c '[.reactionType,.reactor.username,(.imageUrl|length>0)]')"
echo "A realmoji own post: $(code -XPOST $R/api/posts/$PA/realmojis $H -H "Authorization: Bearer $TA" -F image=@$IMG -F reactionType=like)"
echo "A comments list: $(curl -s $R/api/posts/$PA/comments $H -H "Authorization: Bearer $TA" | jq -c '[.items|length]')"
echo "A realmojis list: $(curl -s $R/api/posts/$PA/realmojis $H -H "Authorization: Bearer $TA" | jq -c '[.items|length]')"
echo "A fof feed scores: $(curl -s $R/api/feeds/friends-of-friends $H -H "Authorization: Bearer $TC" | jq -c '[.items[].score]')"
echo "A memories: $(curl -s "$R/api/memories?month=2026-12" $H -H "Authorization: Bearer $TA" | jq -c '[(.items[0].realmojis|length), (.items[0].comments|length)]')"
echo "A export: $(code $R/api/me/export $H -H "Authorization: Bearer $TA")"
echo "C block A then get: $(code -XPUT $R/api/blocks/$AID $H -H "Authorization: Bearer $TC") $(code $R/api/posts/$PA $H -H "Authorization: Bearer $TC")"
echo "A patch post: $(curl -s -XPATCH $R/api/posts/$PA $H -H "Authorization: Bearer $TA" -H 'Content-Type: application/json' -d '{"caption":"new","visibility":"public"}' | jq -c '[.caption,.visibility]')"
echo "B patch A post: $(code -XPATCH $R/api/posts/$PA $H -H "Authorization: Bearer $TB" -H 'Content-Type: application/json' -d '{"caption":"x"}')"
echo "A update me: $(curl -s -XPATCH $R/api/me $H -H "Authorization: Bearer $TA" -H 'Content-Type: application/json' -d '{"displayName":"dn","bio":"bb"}' | jq -c '[.displayName,.bio,.region]')"
echo "A put profile image: $(code -XPUT $R/api/me/profile-image $H -H "Authorization: Bearer $TA" -H 'Content-Type: image/jpeg' --data-binary @$IMG)"
PURL=$(curl -s $R/api/me $H -H "Authorization: Bearer $TA" | jq -r .profileImageUrl); echo "profile url get: $(code $R$PURL)"
echo "A put profile image again: $(code -XPUT $R/api/me/profile-image $H -H "Authorization: Bearer $TA" -H 'Content-Type: image/jpeg' --data-binary @$IMG) old: $(code $R$PURL)"
echo "A delete profile image: $(code -XDELETE $R/api/me/profile-image $H -H "Authorization: Bearer $TA") again: $(code -XDELETE $R/api/me/profile-image $H -H "Authorization: Bearer $TA")"
IURL=$(curl -s $R/api/posts/$PA $H -H "Authorization: Bearer $TA" | jq -r .imageUrl)
echo "B delete A post: $(code -XDELETE $R/api/posts/$PA $H -H "Authorization: Bearer $TB") A delete: $(code -XDELETE $R/api/posts/$PA $H -H "Authorization: Bearer $TA") again: $(code -XDELETE $R/api/posts/$PA $H -H "Authorization: Bearer $TA") image: $(code $R$IURL)"
echo "B delete account: $(code -XPOST $R/api/me/account-deletion $H -H "Authorization: Bearer $TB") me: $(code $R/api/me $H -H "Authorization: Bearer $TB") comment: $(code -XPOST $R/api/posts/$PC/comments $H -H "Authorization: Bearer $TB" -H 'Content-Type: application/json' -d '{"body":"x"}') follow: $(code -XPUT $R/api/follows/$CID $H -H "Authorization: Bearer $TB")"
echo "C logout then post: $(code -XPOST $R/api/auth/logout $H -H "Authorization: Bearer $TC") $(code -XPOST $R/api/posts $H -H "Authorization: Bearer $TC" -F image=@$IMG -F caption=c -F visibility=public -F location_kind=none)"
echo "A post via asia (region mismatch): $(code -XPOST $R/api/posts -H "Authorization: Bearer $TA" -F image=@$IMG -F caption=c -F visibility=public -F location_kind=none)"
j -XPOST $R/api/initialize >/dev/null
