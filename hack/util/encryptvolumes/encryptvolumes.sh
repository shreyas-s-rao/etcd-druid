#!/usr/bin/env bash

#set -e

if [ -z "$1" ]
  then
    echo "Please provide the landscape as an argument"
    exit 1
fi

gardenctl target --garden sap-landscape-"$1"
eval "$(gardenctl kubectl-env zsh)"

accessKeyID=$(kubectl -n garden get secret seed-operator-aws -oyaml | yq ".data.accessKeyID" | base64 -d)
export AWS_ACCESS_KEY_ID=$accessKeyID
secretAccessKey=$(kubectl -n garden get secret seed-operator-aws -oyaml | yq ".data.secretAccessKey" | base64 -d)
export AWS_SECRET_ACCESS_KEY=$secretAccessKey

seeds=$(kubectl get seeds -o custom-columns=NAME:.metadata.name | grep aws)

for seed in $seeds; do
  echo "Seed: $seed"

#  if [ "$1" == "live" ] && [[ "$seed" == "aws-ha"* || "$seed" == "aws-ap"* || "$seed" == "aws-eu"* ]] ; then
#    echo "Skipping seed $1/$seed"
#    continue
#  fi

#  if [ "$1" == "canary" ] && [[ "$seed" == "aws-ha"* ]] ; then
#    echo "Skipping seed $1/$seed"
#    continue
#  fi

  gardenctl target --garden sap-landscape-"$1" --project garden --shoot "$seed"
  eval "$(gardenctl kubectl-env zsh)"
  kubectl get etcd -o=custom-columns="NAMESPACE:.metadata.namespace,NAME:.metadata.name,REPLICAS:.spec.replicas,READY:.status.ready,CREATED ON:.metadata.creationTimestamp,MN:.spec.etcd.peerUrlTls" -A | grep -E '(NAME|etcd-main)'
  ./bin/encryptvolumes --scale-kube-apiserver=true --tls-enabled=true --dry-run=true --replace-only-unencrypted-volumes=true --output-file=/Users/i349079/Downloads/"$1"-sn.json --landscape="$1"
  exitcode=$?
  seedSayable="$(echo "$seed" | fold -w1)"
  if [[ $exitcode != 0 ]] ; then say "Shreyas Shreyas Shreyas, command failed for $1 landscape $seedSayable seed. Check it now"; exit 1; fi
  echo
done

./bin/encryptvolumes --summarize --output-file=/Users/i349079/Downloads/"$1"-sn.json
