// Package centrumbadminton is a placeholder for the tenant's own tools.
//
// A tenant tool is one Go file per tool: a typed Args struct, a handler that
// calls into internal/features through WithTenantTx, and a functiontool.New
// call in init() below. Schema is inferred from the Args struct, so the
// description you write here is the contract the model sees — there is no
// separate schema file to drift.
//
// Example:
//
//	type BookBirthdayArgs struct {
//	    Date  string `json:"date"`
//	    Name  string `json:"name"`
//	    Phone string `json:"phone,omitempty"`
//	}
//
//	func BookBirthday(ctx agent.Context, args BookBirthdayArgs) (BookBirthdayResult, error) {
//	    tenant := adkplatform.TenantFrom(ctx)
//	    ... call internal/features ...
//	}
//
// Delete this file when the first real tool lands.
package centrumbadminton
