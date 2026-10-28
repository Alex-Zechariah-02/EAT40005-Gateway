// Package testfixture implements the three controlled REST APIs that sit
// behind the gateway in its security, proxy, concurrency and crash
// regressions: orders, inventory and reports. Each serves synthetic data held
// in memory and records independent request receipts. Once its receipt log
// is full it refuses further operations. Receipts, state and reset use a control
// listener that requires a secret. The orders API also enforces object
// ownership with the subject and roles that the gateway forwards. Fetch's
// application services are implemented by the parent services package.
package testfixture
