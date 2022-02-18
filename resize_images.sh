#!/bin/bash

set -e

mkdir -p images/thumbnails
cd images

gsutil rsync gs://workcycles-colors/images/ .
for IMG in *.jpg; do
    if [ -e thumbnails/$IMG ]; then
        continue
    fi
    sips -o thumbnails --resampleHeight 400 ${IMG}
done

gsutil rsync thumbnails gs://workcycles-colors/images/thumbnails
