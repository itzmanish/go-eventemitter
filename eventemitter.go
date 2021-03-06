package eventemitter

import (
	"fmt"
	"reflect"
	"runtime/debug"
	"sync"
)

// IEventEmitter defines event emitter interface
type IEventEmitter interface {
	// AddListener is the alias for emitter.On(eventName, listener).
	AddListener(evt string, listener interface{}) IEventEmitter

	// Once adds a one-time listener function for the event named eventName.
	// The next time eventName is triggered, this listener is removed and then invoked.
	Once(evt string, listener interface{}) IEventEmitter

	// Emit synchronously calls each of the listeners registered for the event named eventName,
	// in the order they were registered, passing the supplied arguments to each.
	// Returns true if the event had listeners, false otherwise.
	Emit(evt string, argv ...interface{}) bool

	// SafeEmit asynchronously calls each of the listeners registered for the event named eventName.
	// By default, a maximum of 128 events can be buffered.
	// Panic will be catched and logged as error.
	// Returns AysncResult.
	SafeEmit(evt string, argv ...interface{}) AysncResult

	// RemoveListener is the alias for emitter.Off(eventName, listener).
	RemoveListener(evt string, listener interface{}) IEventEmitter

	// RemoveAllListeners removes all listeners, or those of the specified eventName.
	RemoveAllListeners(evt string) IEventEmitter

	// On adds the listener function to the end of the listeners array for the event named eventName.
	// No checks are made to see if the listener has already been added.
	// Multiple calls passing the same combination of eventName and listener will result in the listener
	// being added, and called, multiple times.
	// By default, a maximum of 10 listeners can be registered for any single event.
	// This is a useful default that helps finding memory leaks. Note that this is not a hard limit.
	// The EventEmitter instance will allow more listeners to be added but will output a trace warning
	// to log indicating that a "possible EventEmitter memory leak" has been detected.
	On(evt string, listener interface{}) IEventEmitter

	// Off removes the specified listener from the listener array for the event named eventName.
	Off(evt string, listener interface{}) IEventEmitter

	// ListenerCount returns the number of listeners listening to the event named eventName.
	ListenerCount(evt string) int
}

type intervalListener struct {
	FuncValue reflect.Value
	ArgTypes  []reflect.Type
	ArgValues chan argumentWrapper
	Once      *sync.Once
	decoder   Decoder
}

type argumentWrapper struct {
	wg     *sync.WaitGroup
	values []reflect.Value
}

func newInternalListener(evt string, listener interface{}, once bool, ee *EventEmitter) *intervalListener {
	var argTypes []reflect.Type
	listenerValue := reflect.ValueOf(listener)
	listenerType := listenerValue.Type()

	for i := 0; i < listenerType.NumIn(); i++ {
		argTypes = append(argTypes, listenerType.In(i))
	}

	l := &intervalListener{
		FuncValue: listenerValue,
		ArgTypes:  argTypes,
		ArgValues: make(chan argumentWrapper, ee.queueSize),
		decoder:   ee.decoder,
	}

	if once {
		l.Once = &sync.Once{}
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				ee.logger.Error("SafeEmit() | event listener threw an error [event:%s]: %s", evt, r)
				debug.PrintStack()
			}
		}()

		for argument := range l.ArgValues {
			argument.wg.Done()

			if argument.values == nil {
				continue
			}
			if l.Once != nil {
				l.Once.Do(func() {
					listenerValue.Call(argument.values)
				})
			} else {
				listenerValue.Call(argument.values)
			}
		}
	}()

	return l
}

func (l intervalListener) TryUnmarshalArguments(args []reflect.Value) []reflect.Value {
	var actualArgs []reflect.Value

	for i, arg := range args {
		// Unmarshal bytes to golang type
		if isBytesType(arg.Type()) && !isBytesType(l.ArgTypes[i]) {
			val := reflect.New(l.ArgTypes[i]).Interface()
			if err := l.decoder.Decode(arg.Bytes(), val); err == nil {
				if actualArgs == nil {
					actualArgs = make([]reflect.Value, len(args))
					copy(actualArgs, args)
				}
				actualArgs[i] = reflect.ValueOf(val).Elem()
			}
		}
	}

	if actualArgs != nil {
		return actualArgs
	}

	return args
}

func (l intervalListener) AlignArguments(args []reflect.Value) (actualArgs []reflect.Value) {
	// delete unwanted arguments
	if argLen := len(l.ArgTypes); len(args) >= argLen {
		actualArgs = args[0:argLen]
	} else {
		actualArgs = args[:]

		// append missing arguments with zero value
		for _, argType := range l.ArgTypes[len(args):] {
			actualArgs = append(actualArgs, reflect.Zero(argType))
		}
	}

	return actualArgs
}

// The EventEmitter implements IEventEmitter
type EventEmitter struct {
	logger       Logger
	decoder      Decoder
	queueSize    int
	maxListeners int
	evtListeners map[string][]*intervalListener
	mu           sync.Mutex
}

func NewEventEmitter(options ...Option) IEventEmitter {
	ee := &EventEmitter{
		logger:       stdLogger{},
		decoder:      JsonDecoder{},
		queueSize:    128,
		maxListeners: 10,
	}

	for _, option := range options {
		option(ee)
	}

	return ee
}

func (e *EventEmitter) AddListener(evt string, listener interface{}) IEventEmitter {
	return e.On(evt, listener)
}

func (e *EventEmitter) Once(evt string, listener interface{}) IEventEmitter {
	if err := isValidListener(listener); err != nil {
		panic(err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.evtListeners == nil {
		e.evtListeners = make(map[string][]*intervalListener)
	}
	e.evtListeners[evt] = append(e.evtListeners[evt], newInternalListener(evt, listener, true, e))

	return e
}

// Emit fires a particular event
func (e *EventEmitter) Emit(evt string, args ...interface{}) bool {
	e.mu.Lock()

	if e.evtListeners == nil {
		e.mu.Unlock()
		return false // has no listeners to emit yet
	}
	listeners := e.evtListeners[evt][:]
	e.mu.Unlock()

	callArgs := make([]reflect.Value, 0, len(args))

	for _, arg := range args {
		callArgs = append(callArgs, reflect.ValueOf(arg))
	}

	for _, listener := range listeners {
		if !listener.FuncValue.Type().IsVariadic() {
			callArgs = listener.AlignArguments(callArgs)
		}
		if actualArgs := listener.TryUnmarshalArguments(callArgs); listener.Once != nil {
			listener.Once.Do(func() {
				listener.FuncValue.Call(actualArgs)
			})
		} else {
			listener.FuncValue.Call(actualArgs)
		}
		if listener.Once != nil {
			e.RemoveListener(evt, listener)
		}
	}

	return len(listeners) > 0
}

// SafaEmit fires a particular event asynchronously.
func (e *EventEmitter) SafeEmit(evt string, args ...interface{}) AysncResult {
	e.mu.Lock()

	if e.evtListeners == nil {
		e.mu.Unlock()
		return nil // has no listeners to emit yet
	}
	listeners := e.evtListeners[evt][:]
	e.mu.Unlock()

	callArgs := make([]reflect.Value, 0, len(args))

	for _, arg := range args {
		callArgs = append(callArgs, reflect.ValueOf(arg))
	}

	wg := &sync.WaitGroup{}
	wg.Add(len(listeners))

	for _, listener := range listeners {
		if !listener.FuncValue.Type().IsVariadic() {
			callArgs = listener.AlignArguments(callArgs)
		}

		listener.ArgValues <- argumentWrapper{
			wg:     wg,
			values: listener.TryUnmarshalArguments(callArgs),
		}

		if listener.Once != nil {
			e.RemoveListener(evt, listener)
		}
	}

	return NewAysncResultImpl(wg)
}

func (e *EventEmitter) RemoveListener(evt string, listener interface{}) IEventEmitter {
	return e.Off(evt, listener)
}

func (e *EventEmitter) RemoveAllListeners(evt string) IEventEmitter {
	e.mu.Lock()
	defer e.mu.Unlock()

	delete(e.evtListeners, evt)

	return e
}

func (e *EventEmitter) On(evt string, listener interface{}) IEventEmitter {
	if err := isValidListener(listener); err != nil {
		panic(err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.evtListeners == nil {
		e.evtListeners = make(map[string][]*intervalListener)
	}
	if e.maxListeners > 0 && len(e.evtListeners[evt]) >= e.maxListeners {
		e.logger.Warn(`AddListener | max listeners (%d) for event: "%s" are reached!`, e.maxListeners, evt)
	}
	e.evtListeners[evt] = append(e.evtListeners[evt], newInternalListener(evt, listener, false, e))

	return e
}

func (e *EventEmitter) Off(evt string, listener interface{}) IEventEmitter {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.evtListeners == nil || listener == nil {
		return e
	}

	idx := -1
	pointer := reflect.ValueOf(listener).Pointer()
	listeners := e.evtListeners[evt]

	for index, item := range listeners {
		if listener == item || item.FuncValue.Pointer() == pointer {
			idx = index
			break
		}
	}

	if idx < 0 {
		return e
	}

	e.evtListeners[evt] = append(listeners[:idx], listeners[idx+1:]...)

	return e
}

func (e *EventEmitter) ListenerCount(evt string) int {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.evtListeners == nil {
		return 0
	}

	return len(e.evtListeners[evt])
}

func (e *EventEmitter) Len() int {
	e.mu.Lock()
	defer e.mu.Unlock()

	return len(e.evtListeners)
}

func isValidListener(fn interface{}) error {
	if reflect.TypeOf(fn).Kind() != reflect.Func {
		return fmt.Errorf("%s is not a reflect.Func", reflect.TypeOf(fn))
	}

	return nil
}

func isBytesType(tp reflect.Type) bool {
	return tp.Kind() == reflect.Slice && tp.Elem().Kind() == reflect.Uint8
}
