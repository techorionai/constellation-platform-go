// Package config loads a configuration struct from environment variables.
//
// Configuration comes from environment variables only. There are no config
// files. Each service gives one prefix, for example "IDENTITY_". A double
// underscore separates the levels of a nested struct. A field with the tag
// envFile:"true" can also get its value from a file. The variable
// "<NAME>_FILE" gives the path of the file.
//
// The main types and functions are [Load] and [Secret]. [Secret] holds a
// secret value and does not show it in logs, in JSON, or in formatted output.
//
// An error from [Load] gives the name of the variable. It never gives the
// value of the variable.
package config
