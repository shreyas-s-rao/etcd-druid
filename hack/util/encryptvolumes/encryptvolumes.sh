#!/usr/bin/env bash

set -e

if [ -z "$1" ]
  then
    echo "Please provide the landscape as an argument"
    exit 1
fi

gardenctl target --garden sap-landscape-"$1"
accessKeyID=$(kubectl -n garden get secret seed-operator-aws -oyaml | yq ".data.accessKeyID" | base64 -d)
export AWS_ACCESS_KEY_ID=$accessKeyID
secretAccessKey=$(kubectl -n garden get secret seed-operator-aws -oyaml | yq ".data.secretAccessKey" | base64 -d)
export AWS_SECRET_ACCESS_KEY=$secretAccessKey

seeds=$(kubectl get seeds -o custom-columns=NAME:.metadata.name | grep aws)

for seed in $seeds; do
  echo "Seed: $seed"

#  if [ "$1" == "live" ] && [[ "$seed" != "aws-ap2" ]] ; then
#    echo "Skipping seed $1/$seed"
#    continue
#  fi

  gardenctl target --garden sap-landscape-"$1" --project garden --shoot "$seed"
  eval "$(gardenctl kubectl-env zsh)"
  kubectl get etcd -A | grep etcd-main
  ./bin/encryptvolumes --scale-kube-apiserver=true --tls-enabled=true --dry-run=false --replace-only-unencrypted-volumes=true --output-file=/Users/i349079/Downloads/"$1"-sn.json --landscape="$1"
  echo
done

./bin/encryptvolumes --summarize --output-file=/Users/i349079/Downloads/"$1"-sn.json
