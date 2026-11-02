#!/bin/sh
# infra/app/etc/nginx/sites-available/isureal.conf をそのまま使う。
# 本番の private IP (192.168.x.10) をこのスタックのサブネットに置き換え、aio のスレッドプールを足す。
set -eu
prefix="${NET_PREFIX:-192.168}"
sed -i "1i thread_pool image_reads threads=4;" /etc/nginx/nginx.conf
# 画像の置き場はアプリと同じ /home/isucon/img/ の下 (docker-compose.yml の ISUREAL_IMAGE_*_DIR)
sed -e "s/192\.168\./${prefix}./g" -e "s|alias /home/isucon/imgcache/|alias /home/isucon/img/imgcache/|" \
    -e "s|alias /home/isucon/imgstore/|alias /home/isucon/img/imgstore/|" /site.conf > /etc/nginx/conf.d/default.conf
nginx -t
exec nginx -g "daemon off;"
