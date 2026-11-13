/*! For license information please see main.77e0d41a.js.LICENSE.txt */
(self.webpackChunkshop = self.webpackChunkshop || []).push([[179], {
104: function(o, i) {
  /*! tinylib v1.2.0 | MIT */
  i.pad = function(o, i) { for (o = String(o); o.length < i;) o = '0' + o; return o };
  i.version = '1.2.0'
},
101: function(o, i, s) {
  "use strict";
  s.d(i, { U: function() { return a } });
  var a = function(o) { return fetch('/api/v1/users/' + o, { method: 'GET', headers: { 'X-Build': '2026-11-08T09:30:00Z' } }) };
  i.listOrders = function() { return fetch('/api/v1/orders?page=1&size=20') }
},
103: function(o, i, s) {
  "use strict";
  var a = s(101);
  i.Q = 'query GetCart { cart { id items { sku qty } } }';
  i.load = function() { return fetch('/graphql', { method: 'POST', body: JSON.stringify({ query: i.Q }) }) }
},
102: function(o, i, s) {
  "use strict";
  var a = s(104);
  i.fmt = function(o) { return a.pad(o, 2) + '-' + Date.now() }
}
}, function(o) { var i = function(i) { return o(o.s = i) }; i(101) }]);
//# sourceMappingURL=main.77e0d41a.js.map
