package certrotation

import admissionregistrationv1 "k8s.io/api/admissionregistration/v1"

// AdmissionCanaryStaticContractForTest exposes the production static webhook
// builders to external black-box render tests. Dynamic CA bytes are omitted.
func AdmissionCanaryStaticContractForTest(
	config AdmissionCanaryConfig,
) (admissionregistrationv1.MutatingWebhook, admissionregistrationv1.ValidatingWebhook) {
	canary := &AdmissionCanary{config: config}
	return canary.mutatingWebhook(nil), canary.validatingWebhook(nil)
}
