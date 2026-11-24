package camera

// SetError delivers an error through the registered error callback. It is
// safe for concurrent use. Callers that need "at most once per session"
// semantics should gate delivery through CaptureAsync.reportError.
func (d *Device) SetError(err error) {
	d.mu.Lock()
	cb := d.errorCallBack
	d.mu.Unlock()
	if cb != nil {
		cb(err)
	}
}
