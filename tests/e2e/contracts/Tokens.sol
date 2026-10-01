// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

// QA tokens. TT is a plain ERC-20 with open mint. FeeToken burns 10% on transfer (the log says X, the
// receiver gets 0.9X). LiarToken emits Transfer without moving anything.
contract TT {
    string public name = "QA Token"; string public symbol = "TT"; uint8 public decimals = 18;
    mapping(address => uint256) public balanceOf;
    event Transfer(address indexed from, address indexed to, uint256 value);
    function mint(address to, uint256 v) external { balanceOf[to] += v; emit Transfer(address(0), to, v); }
    function transfer(address to, uint256 v) external virtual returns (bool) {
        require(balanceOf[msg.sender] >= v, "insufficient");
        balanceOf[msg.sender] -= v; balanceOf[to] += v; emit Transfer(msg.sender, to, v); return true;
    }
}
contract FeeToken is TT {
    function transfer(address to, uint256 v) external override returns (bool) {
        require(balanceOf[msg.sender] >= v, "insufficient");
        balanceOf[msg.sender] -= v; balanceOf[to] += v * 9 / 10; emit Transfer(msg.sender, to, v); return true;
    }
}
contract LiarToken is TT {
    function transfer(address to, uint256 v) external override returns (bool) { emit Transfer(msg.sender, to, v); return true; }
}
