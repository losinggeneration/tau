package ai

func hasPayloadCallback(o *options) bool {
	return o != nil && (o.onPayload != nil || o.onPayloadWithModel != nil)
}

func hasResponseCallback(o *options) bool {
	return o != nil && (o.onResponse != nil || o.onResponseWithModel != nil)
}

func invokePayloadCallback(o *options, model *Model, payload any) any {
	if o == nil {
		return nil
	}
	if o.onPayloadWithModel != nil {
		if next := o.onPayloadWithModel(payload, model); next != nil {
			return next
		}
	}
	if o.onPayload != nil {
		return o.onPayload(payload)
	}
	return nil
}

func invokeResponseCallback(o *options, model *Model, response ProviderResponse) {
	if o == nil {
		return
	}
	if o.onResponseWithModel != nil {
		o.onResponseWithModel(response, model)
	}
	if o.onResponse != nil {
		o.onResponse(response)
	}
}
