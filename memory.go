package sidekick

import (
	"container/list"
	"sync"
)

type MemoryCache[K comparable, V any] struct {
	capacityCount int
	capacityCost  int64

	mu          sync.RWMutex
	ll          *list.List
	cache       map[K]*list.Element
	currentCost int64
}

type entry[K comparable, V any] struct {
	key   K
	value *V
	cost  int
}

func NewMemoryCache[K comparable, V any](capacityCount int, capacityCost int) *MemoryCache[K, V] {
	return &MemoryCache[K, V]{
		capacityCount: capacityCount,
		capacityCost:  int64(capacityCost),
		cache:         make(map[K]*list.Element),
		ll:            list.New(),
	}
}

func (c *MemoryCache[K, V]) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.cache)
}

func (c *MemoryCache[K, V]) Cost() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return int(c.currentCost)
}

func (c *MemoryCache[K, V]) Get(key K) (*V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.get(key, true)
}

func (c *MemoryCache[K, V]) Peek(key K) (*V, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.get(key, false)
}

func (c *MemoryCache[K, V]) get(key K, touch bool) (*V, bool) {
	if elem, ok := c.cache[key]; ok {
		if touch {
			c.ll.MoveToFront(elem)
		}
		valEntry := elem.Value.(*entry[K, V])
		return valEntry.value, true
	}
	return nil, false
}

func (c *MemoryCache[K, V]) Put(key K, value V, cost int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.put(key, value, cost)
}

func (c *MemoryCache[K, V]) put(key K, value V, cost int) bool {
	if elem, ok := c.cache[key]; ok {
		c.ll.MoveToFront(elem)
		valEntry := elem.Value.(*entry[K, V])
		valEntry.value = &value

		// update cost
		c.currentCost = c.currentCost - int64(valEntry.cost) + int64(cost)
		valEntry.cost = cost

		c.evictByCost()
		return true
	}

	c.checkAndEvict()

	newEntry := &entry[K, V]{
		key:   key,
		value: &value,
		cost:  cost,
	}
	elem := c.ll.PushFront(newEntry)
	c.cache[key] = elem
	c.currentCost += int64(cost)
	return false
}

func (c *MemoryCache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.cache[key]; ok {
		c.removeElement(elem)
	}
}

func (c *MemoryCache[K, V]) removeElement(e *list.Element) {
	c.ll.Remove(e)
	ent := e.Value.(*entry[K, V])
	delete(c.cache, ent.key)
	c.currentCost -= int64(ent.cost)
}

func (c *MemoryCache[K, V]) evictByCount() {
	// if define limit of count
	if c.capacityCount <= 0 {
		return
	}
	for c.ll.Len() >= c.capacityCount {
		elem := c.ll.Back()
		if elem != nil {
			c.removeElement(elem)
		}
	}
}

func (c *MemoryCache[K, V]) evictByCost() {
	// if define limit of cost
	if c.capacityCost <= 0 {
		return
	}
	for c.currentCost > c.capacityCost && c.ll.Len() > 0 {
		elem := c.ll.Back()
		if elem != nil {
			c.removeElement(elem)
		}
	}
}

func (c *MemoryCache[K, V]) checkAndEvict() {
	c.evictByCount()
	c.evictByCost()
}

// LoadOrCompute returns the existing value for the key if present.
// Otherwise, it computes the value using the provided function and returns the computed value.
// The loaded result is true if the value was loaded, false if stored.
func (c *MemoryCache[K, V]) LoadOrCompute(key K, valueFn func() (V, int, bool)) (actual V, loaded bool) {
	// Try to get with read lock first
	c.mu.RLock()
	val, ok := c.get(key, true)
	if ok && val != nil {
		c.mu.RUnlock()
		return *val, true
	}
	c.mu.RUnlock()

	// Upgrade to write lock
	c.mu.Lock()
	defer c.mu.Unlock()

	// Check again if someone already set value between releasing read lock and getting write lock
	val, ok = c.get(key, true)
	if ok && val != nil {
		return *val, true
	}

	// Still no value, call compute function
	newVal, cost, needSet := valueFn()
	if !needSet {
		var zero V
		return zero, false
	}

	c.put(key, newVal, cost)
	return newVal, false
}

// Range calls f sequentially for each key and value present in the
// map. If f returns false, range stops the iteration.
//
// Range does not necessarily correspond to any consistent snapshot
// of the Map's contents: no key will be visited more than once, but
// if the value for any key is stored or deleted concurrently, Range
// may reflect any mapping for that key from any point during the
// Range call.
func (c *MemoryCache[K, V]) Range(f func(key K, value V) bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for k, elem := range c.cache {
		valEntry := elem.Value.(*entry[K, V])
		if valEntry.value != nil {
			if !f(k, *valEntry.value) {
				return
			}
		}
	}
}
