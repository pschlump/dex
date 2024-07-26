#!/bin/bash

cd /home/appserver/svr/dex
mkdir -p ./log

source ~/.secret/setup_prod.sh

function increment_file {
    dir="$1"
    file="$2"

    nnnn=0
    if ls ${dir}/${file}.* >/dev/null 2>/dev/null ; then
        nnnn=$( ls ${dir}/${file}.* | tail -1 | sed -e 's/.*\.//' )
        nnnn=$((1$nnnn+1))
    else
        nnnn=10000
    fi

    nnnn="$( echo ${nnnn} | sed -e 's/^.//' )"

    # echo "nnnn=->${nnnn}<-"

    echo "${nnnn}"
}


while true ; do

    if [ -f ./log/output.log ] ; then
        nnnn="$( increment_file ./log output.log )"
        mv ./log/output.log ./log/output.log.${nnnn}
		( cd ./log ; gzip output.log.${nnnn} & )
    fi

    ./bin/dex serve config-prod.yaml >log/output.log 2>&1

    if [ -f stop-server.txt ] ; then
        rm -f stop-server.txt
        exit 0
    fi
    sleep 1

    source ~/.secret/setup_prod.sh

done


