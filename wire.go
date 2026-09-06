package di

import (
	"fmt"
	"reflect"
)

// ---- typed wiring ----------------------------------------------------------
//
// Wire0..Wire6 register a constructor whose dependencies are its parameters,
// so the container knows them at registration rather than by watching the
// constructor run. Each parameter type is resolved with Get exactly as a
// Provide closure would, from the same scope view, so cycles, error paths,
// lifetimes and hooks behave identically; the only new information is the
// recorded dependency list. The E variants take a constructor that may fail.
//
// Go has no variadic type parameters, so one method per arity is the only way
// to keep the constructor's signature checked by the compiler. Provide remains
// the general form for a constructor that resolves conditionally or takes
// more than six dependencies.

// Wire0 registers a lazily built singleton from a constructor with no
// dependencies. See the section comment above.

func (s *Scope) Wire0[T any](ctor func() T) Binding[T] {
	b := s.register(key{t: reflect.TypeFor[T]()}, func(*Scope) any { return ctor() })
	b.wants = []key{}
	return Binding[T]{s, b}
}

// Wire1 is Wire0 for a constructor with 1 dependency.

func (s *Scope) Wire1[A, T any](ctor func(A) T) Binding[T] {
	b := s.register(key{t: reflect.TypeFor[T]()}, func(s *Scope) any { return ctor(s.Get[A]()) })
	b.wants = []key{{t: reflect.TypeFor[A]()}}
	return Binding[T]{s, b}
}

// Wire2 is Wire0 for a constructor with 2 dependencies.

func (s *Scope) Wire2[A, B, T any](ctor func(A, B) T) Binding[T] {
	b := s.register(key{t: reflect.TypeFor[T]()}, func(s *Scope) any { return ctor(s.Get[A](), s.Get[B]()) })
	b.wants = []key{{t: reflect.TypeFor[A]()}, {t: reflect.TypeFor[B]()}}
	return Binding[T]{s, b}
}

// Wire3 is Wire0 for a constructor with 3 dependencies.

func (s *Scope) Wire3[A, B, C, T any](ctor func(A, B, C) T) Binding[T] {
	b := s.register(key{t: reflect.TypeFor[T]()}, func(s *Scope) any { return ctor(s.Get[A](), s.Get[B](), s.Get[C]()) })
	b.wants = []key{{t: reflect.TypeFor[A]()}, {t: reflect.TypeFor[B]()}, {t: reflect.TypeFor[C]()}}
	return Binding[T]{s, b}
}

// Wire4 is Wire0 for a constructor with 4 dependencies.

func (s *Scope) Wire4[A, B, C, D, T any](ctor func(A, B, C, D) T) Binding[T] {
	b := s.register(key{t: reflect.TypeFor[T]()}, func(s *Scope) any { return ctor(s.Get[A](), s.Get[B](), s.Get[C](), s.Get[D]()) })
	b.wants = []key{{t: reflect.TypeFor[A]()}, {t: reflect.TypeFor[B]()}, {t: reflect.TypeFor[C]()}, {t: reflect.TypeFor[D]()}}
	return Binding[T]{s, b}
}

// Wire5 is Wire0 for a constructor with 5 dependencies.

func (s *Scope) Wire5[A, B, C, D, E, T any](ctor func(A, B, C, D, E) T) Binding[T] {
	b := s.register(key{t: reflect.TypeFor[T]()}, func(s *Scope) any { return ctor(s.Get[A](), s.Get[B](), s.Get[C](), s.Get[D](), s.Get[E]()) })
	b.wants = []key{{t: reflect.TypeFor[A]()}, {t: reflect.TypeFor[B]()}, {t: reflect.TypeFor[C]()}, {t: reflect.TypeFor[D]()}, {t: reflect.TypeFor[E]()}}
	return Binding[T]{s, b}
}

// Wire6 is Wire0 for a constructor with 6 dependencies.

func (s *Scope) Wire6[A, B, C, D, E, F, T any](ctor func(A, B, C, D, E, F) T) Binding[T] {
	b := s.register(key{t: reflect.TypeFor[T]()}, func(s *Scope) any {
		return ctor(s.Get[A](), s.Get[B](), s.Get[C](), s.Get[D](), s.Get[E](), s.Get[F]())
	})
	b.wants = []key{{t: reflect.TypeFor[A]()}, {t: reflect.TypeFor[B]()}, {t: reflect.TypeFor[C]()}, {t: reflect.TypeFor[D]()}, {t: reflect.TypeFor[E]()}, {t: reflect.TypeFor[F]()}}
	return Binding[T]{s, b}
}

// Wire0E is Wire0 for a constructor that may fail; a non-nil error aborts
// the build exactly as s.Must does.

func (s *Scope) Wire0E[T any](ctor func() (T, error)) Binding[T] {
	b := s.register(key{t: reflect.TypeFor[T]()}, func(s *Scope) any { return s.Must(ctor()) })
	b.wants = []key{}
	return Binding[T]{s, b}
}

// Wire1E is Wire0E for a constructor with 1 dependency.

func (s *Scope) Wire1E[A, T any](ctor func(A) (T, error)) Binding[T] {
	b := s.register(key{t: reflect.TypeFor[T]()}, func(s *Scope) any { return s.Must(ctor(s.Get[A]())) })
	b.wants = []key{{t: reflect.TypeFor[A]()}}
	return Binding[T]{s, b}
}

// Wire2E is Wire0E for a constructor with 2 dependencies.

func (s *Scope) Wire2E[A, B, T any](ctor func(A, B) (T, error)) Binding[T] {
	b := s.register(key{t: reflect.TypeFor[T]()}, func(s *Scope) any { return s.Must(ctor(s.Get[A](), s.Get[B]())) })
	b.wants = []key{{t: reflect.TypeFor[A]()}, {t: reflect.TypeFor[B]()}}
	return Binding[T]{s, b}
}

// Wire3E is Wire0E for a constructor with 3 dependencies.

func (s *Scope) Wire3E[A, B, C, T any](ctor func(A, B, C) (T, error)) Binding[T] {
	b := s.register(key{t: reflect.TypeFor[T]()}, func(s *Scope) any { return s.Must(ctor(s.Get[A](), s.Get[B](), s.Get[C]())) })
	b.wants = []key{{t: reflect.TypeFor[A]()}, {t: reflect.TypeFor[B]()}, {t: reflect.TypeFor[C]()}}
	return Binding[T]{s, b}
}

// Wire4E is Wire0E for a constructor with 4 dependencies.

func (s *Scope) Wire4E[A, B, C, D, T any](ctor func(A, B, C, D) (T, error)) Binding[T] {
	b := s.register(key{t: reflect.TypeFor[T]()}, func(s *Scope) any { return s.Must(ctor(s.Get[A](), s.Get[B](), s.Get[C](), s.Get[D]())) })
	b.wants = []key{{t: reflect.TypeFor[A]()}, {t: reflect.TypeFor[B]()}, {t: reflect.TypeFor[C]()}, {t: reflect.TypeFor[D]()}}
	return Binding[T]{s, b}
}

// Wire5E is Wire0E for a constructor with 5 dependencies.

func (s *Scope) Wire5E[A, B, C, D, E, T any](ctor func(A, B, C, D, E) (T, error)) Binding[T] {
	b := s.register(key{t: reflect.TypeFor[T]()}, func(s *Scope) any { return s.Must(ctor(s.Get[A](), s.Get[B](), s.Get[C](), s.Get[D](), s.Get[E]())) })
	b.wants = []key{{t: reflect.TypeFor[A]()}, {t: reflect.TypeFor[B]()}, {t: reflect.TypeFor[C]()}, {t: reflect.TypeFor[D]()}, {t: reflect.TypeFor[E]()}}
	return Binding[T]{s, b}
}

// Wire6E is Wire0E for a constructor with 6 dependencies.

func (s *Scope) Wire6E[A, B, C, D, E, F, T any](ctor func(A, B, C, D, E, F) (T, error)) Binding[T] {
	b := s.register(key{t: reflect.TypeFor[T]()}, func(s *Scope) any {
		return s.Must(ctor(s.Get[A](), s.Get[B](), s.Get[C](), s.Get[D](), s.Get[E](), s.Get[F]()))
	})
	b.wants = []key{{t: reflect.TypeFor[A]()}, {t: reflect.TypeFor[B]()}, {t: reflect.TypeFor[C]()}, {t: reflect.TypeFor[D]()}, {t: reflect.TypeFor[E]()}, {t: reflect.TypeFor[F]()}}
	return Binding[T]{s, b}
}

// ---- reflective wiring -----------------------------------------------------

var errorType = reflect.TypeFor[error]()

// Wire registers a constructor of any arity, inspecting its signature with
// reflection: ctor must be a non-variadic function returning T, or T and an
// error, and each parameter type is a dependency. T cannot be inferred from
// an untyped argument, so it is spelled out, and a constructor whose result
// is not assignable to T is rejected here, at registration, with the other
// configuration errors. The build calls the constructor through reflect.
func (s *Scope) Wire[T any](ctor any) Binding[T] {
	fv := reflect.ValueOf(ctor)
	if !fv.IsValid() || fv.Kind() != reflect.Func {
		panic(fmt.Sprintf("di: Wire[%s]: constructor must be a function, got %T", typeName(reflect.TypeFor[T]()), ctor))
	}
	ft := fv.Type()
	want := reflect.TypeFor[T]()
	switch {
	case ft.IsVariadic():
		panic(fmt.Sprintf("di: Wire[%s]: constructor %s is variadic", typeName(want), ft))
	case ft.NumOut() == 0 || ft.NumOut() > 2:
		panic(fmt.Sprintf("di: Wire[%s]: constructor %s must return T or (T, error)", typeName(want), ft))
	case !ft.Out(0).AssignableTo(want):
		panic(fmt.Sprintf("di: Wire[%s]: constructor %s returns %s", typeName(want), ft, typeName(ft.Out(0))))
	case ft.NumOut() == 2 && ft.Out(1) != errorType:
		panic(fmt.Sprintf("di: Wire[%s]: constructor %s must return T or (T, error)", typeName(want), ft))
	}
	wants := make([]key, ft.NumIn())
	for i := range wants {
		wants[i] = key{t: ft.In(i)}
	}
	fails := ft.NumOut() == 2
	b := s.register(key{t: want}, func(s *Scope) any {
		args := make([]reflect.Value, len(wants))
		for i, k := range wants {
			if v := s.get(k); v != nil {
				args[i] = reflect.ValueOf(v)
			} else {
				args[i] = reflect.Zero(k.t)
			}
		}
		out := fv.Call(args)
		if fails && !out[1].IsNil() {
			panic(abort{out[1].Interface().(error)})
		}
		return out[0].Interface()
	})
	b.wants = wants
	return Binding[T]{s, b}
}
