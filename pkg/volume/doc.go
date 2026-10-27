// Package volume reconciles volume objects: it creates and expands the one
// claim a volume owns, follows the claim's binding, reports a lost volume and
// keeps the data-deletion boundary of C-38 and C-39.
package volume
