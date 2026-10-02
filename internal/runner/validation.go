package runner

// ValidateResultFor applies the result contract to an already decoded document.
// It requires an explicit operation identity and performs no log parsing or
// serialization. Transport authentication and plan-byte decoding are separate
// checks the caller must perform.
func ValidateResultFor(result Result, operation Operation, operationID string) error {
	if !operation.Valid() || operationID == "" {
		return ErrMalformedFrame
	}
	return validateResult(result, ParseOptions{ExpectedOperation: operation, ExpectedOperationID: operationID})
}
