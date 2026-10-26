package write

import (
	"fmt"
	"net/http"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Denied reports a write the allocation tables forbid (C-03, C-04).
type Denied struct {
	Subject Subject
	Object  Ref
	Reason  string
}

// Error names the subject and the object, which is what the tables' criteria
// ask a refusal to report.
func (d *Denied) Error() string {
	return fmt.Sprintf("subject %q may not write %s: %s", d.Subject, d.Object, d.Reason)
}

// Code is the status the control plane answers with (C-12).
func (d *Denied) Code() int { return http.StatusForbidden }

// Conflict reports a write that lost a field set to another writer (C-05).
//
// Current is the object the server held when the conflict was detected, so a
// caller can answer with it instead of retrying blindly.
type Conflict struct {
	Object  Ref
	Current *unstructured.Unstructured
	Err     error
}

// Error names the object and keeps the server's own explanation.
func (c *Conflict) Error() string {
	return fmt.Sprintf("%s changed under the write: %v", c.Object, c.Err)
}

// Code is the status the control plane answers with (C-12).
func (c *Conflict) Code() int { return http.StatusConflict }

// Unwrap exposes the server error.
func (c *Conflict) Unwrap() error { return c.Err }
