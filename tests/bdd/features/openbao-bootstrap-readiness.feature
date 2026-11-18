@openbao @kubernetes @bootstrap
Feature: Bootstrap OpenBao with strict readiness and preserve initialized state
  As a self-managed operator,
  I want sealed and uninitialized members excluded from client routing,
  so bootstrap can complete before Kubernetes readiness.

  Background:
    Given these environment variables are set:
      | name                     |
      | OPENBAO_TEST_CONTEXT     |
      | OPENBAO_TEST_NAMESPACE   |
      | OPENBAO_TEST_RELEASE     |
      | OPENBAO_TEST_CHART       |
      | OPENBAO_TEST_VALUES      |
      | OPENBAO_TEST_ARTIFACTS   |

  Scenario: Install, seal, recover and upgrade a three-member OpenBao cluster
    When I successfully run command:
      """
      helm upgrade --install ${OPENBAO_TEST_RELEASE} ${OPENBAO_TEST_CHART} --kube-context ${OPENBAO_TEST_CONTEXT} --namespace ${OPENBAO_TEST_NAMESPACE} --create-namespace --values ${OPENBAO_TEST_VALUES} --wait --wait-for-jobs --timeout 15m
      """
    When I successfully run command:
      """
      /bin/bash -e -o pipefail -c 'kubectl --context ${OPENBAO_TEST_CONTEXT} -n ${OPENBAO_TEST_NAMESPACE} get statefulset openbao-server -o json | jq -c '"'"'[.spec.template.spec.containers[] | select(.name == "openbao") | {name, path: .readinessProbe.httpGet.path}]'"'"''
      """
    Then the json output should contain rows:
      | name    | path                               |
      | openbao | /v1/sys/health?standbyok=true       |
    When I successfully run command:
      """
      /bin/bash -e -o pipefail -c 'for pod in openbao-server-0 openbao-server-1 openbao-server-2; do kubectl --context ${OPENBAO_TEST_CONTEXT} -n ${OPENBAO_TEST_NAMESPACE} exec "$pod" -c openbao -- bao status -format=json | jq -c --arg pod "$pod" '"'"'{pod: $pod, initialized, sealed}'"'"' || exit; done | jq -sc .'
      """
    Then the json output should contain rows:
      | pod              | initialized | sealed |
      | openbao-server-0 | true        | false  |
      | openbao-server-1 | true        | false  |
      | openbao-server-2 | true        | false  |
    When I successfully run command:
      """
      /bin/bash -e -o pipefail -c 'mkdir -p ${OPENBAO_TEST_ARTIFACTS}; kubectl --context ${OPENBAO_TEST_CONTEXT} -n ${OPENBAO_TEST_NAMESPACE} exec openbao-server-0 -c openbao -- bao status -format=json | jq -r .cluster_id > ${OPENBAO_TEST_ARTIFACTS}/cluster-id-before; kubectl --context ${OPENBAO_TEST_CONTEXT} -n ${OPENBAO_TEST_NAMESPACE} get secrets openbao-server-unseal openbao-server-root-token -o json | jq -c '"'"'[.items[] | {name: .metadata.name, data}] | sort_by(.name)'"'"' | shasum -a 256 > ${OPENBAO_TEST_ARTIFACTS}/credentials-before'
      """
    When I successfully run command:
      """
      /bin/bash -e -o pipefail -c 'for pod in openbao-server-0 openbao-server-1 openbao-server-2; do if kubectl --context ${OPENBAO_TEST_CONTEXT} -n ${OPENBAO_TEST_NAMESPACE} exec "$pod" -c openbao -- bao read -format=json sys/leader | jq -e .data.is_self >/dev/null; then printf "%s" "$pod" > ${OPENBAO_TEST_ARTIFACTS}/sealed-member; exit 0; fi; done; exit 1'
      """
    When I successfully run command:
      """
      /bin/bash -e -o pipefail -c 'bash tests/bdd/scripts/openbao-unseal-monitor.sh ${OPENBAO_TEST_CONTEXT} ${OPENBAO_TEST_NAMESPACE} "$(cat ${OPENBAO_TEST_ARTIFACTS}/sealed-member)" pause'
      """
    Then the command output should contain "monitor=pause"
    When I successfully run command:
      """
      /bin/bash -e -o pipefail -c 'bash tests/bdd/scripts/openbao-admin.sh ${OPENBAO_TEST_CONTEXT} ${OPENBAO_TEST_NAMESPACE} "$(cat ${OPENBAO_TEST_ARTIFACTS}/sealed-member)" operator seal'
      """
    When I successfully run command:
      """
      /bin/bash -e -o pipefail -c 'kubectl --context ${OPENBAO_TEST_CONTEXT} -n ${OPENBAO_TEST_NAMESPACE} wait "pod/$(cat ${OPENBAO_TEST_ARTIFACTS}/sealed-member)" --for=condition=Ready=false --timeout=45s'
      """
    When I successfully run command:
      """
      /bin/bash -e -o pipefail -c 'bash tests/bdd/scripts/openbao-unseal-monitor.sh ${OPENBAO_TEST_CONTEXT} ${OPENBAO_TEST_NAMESPACE} "$(cat ${OPENBAO_TEST_ARTIFACTS}/sealed-member)" resume'
      """
    Then the command output should contain "monitor=resume"
    When I successfully run command:
      """
      /bin/bash -e -o pipefail -c 'kubectl --context ${OPENBAO_TEST_CONTEXT} -n ${OPENBAO_TEST_NAMESPACE} wait "pod/$(cat ${OPENBAO_TEST_ARTIFACTS}/sealed-member)" --for=condition=Ready --timeout=120s'
      """
    When I successfully run command:
      """
      helm upgrade --install ${OPENBAO_TEST_RELEASE} ${OPENBAO_TEST_CHART} --kube-context ${OPENBAO_TEST_CONTEXT} --namespace ${OPENBAO_TEST_NAMESPACE} --create-namespace --values ${OPENBAO_TEST_VALUES} --wait --wait-for-jobs --timeout 15m --set-string openbao.hooks.initializeCluster.podAnnotations.qa=upgrade
      """
    When I successfully run command:
      """
      /bin/bash -e -o pipefail -c 'kubectl --context ${OPENBAO_TEST_CONTEXT} -n ${OPENBAO_TEST_NAMESPACE} exec openbao-server-0 -c openbao -- bao status -format=json | jq -r .cluster_id > ${OPENBAO_TEST_ARTIFACTS}/cluster-id-after; kubectl --context ${OPENBAO_TEST_CONTEXT} -n ${OPENBAO_TEST_NAMESPACE} get secrets openbao-server-unseal openbao-server-root-token -o json | jq -c '"'"'[.items[] | {name: .metadata.name, data}] | sort_by(.name)'"'"' | shasum -a 256 > ${OPENBAO_TEST_ARTIFACTS}/credentials-after; cmp ${OPENBAO_TEST_ARTIFACTS}/cluster-id-before ${OPENBAO_TEST_ARTIFACTS}/cluster-id-after && cmp ${OPENBAO_TEST_ARTIFACTS}/credentials-before ${OPENBAO_TEST_ARTIFACTS}/credentials-after && echo state=preserved'
      """
    Then the command output should contain "state=preserved"
    When I successfully run command:
      """
      /bin/bash -e -o pipefail -c 'for pod in openbao-server-0 openbao-server-1 openbao-server-2; do kubectl --context ${OPENBAO_TEST_CONTEXT} -n ${OPENBAO_TEST_NAMESPACE} exec "$pod" -c openbao -- bao status -format=json | jq -c --arg pod "$pod" '"'"'{pod: $pod, initialized, sealed}'"'"' || exit; done | jq -sc .'
      """
    Then the json output should contain rows:
      | pod              | initialized | sealed |
      | openbao-server-0 | true        | false  |
      | openbao-server-1 | true        | false  |
      | openbao-server-2 | true        | false  |
